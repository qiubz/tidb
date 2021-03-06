// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package ddl

import (
	"github.com/juju/errors"
	"github.com/pingcap/tidb/infoschema"
	"github.com/pingcap/tidb/meta"
	"github.com/pingcap/tidb/meta/autoid"
	"github.com/pingcap/tidb/model"
	"github.com/pingcap/tidb/table"
	"github.com/pingcap/tidb/tablecodec"
	"github.com/pingcap/tidb/terror"
)

func (d *ddl) onCreateTable(t *meta.Meta, job *model.Job) error {
	schemaID := job.SchemaID
	tbInfo := &model.TableInfo{}
	if err := job.DecodeArgs(tbInfo); err != nil {
		// arg error, cancel this job.
		job.State = model.JobCancelled
		return errors.Trace(err)
	}

	tbInfo.State = model.StateNone

	tables, err := t.ListTables(schemaID)
	if terror.ErrorEqual(err, meta.ErrDBNotExists) {
		job.State = model.JobCancelled
		return errors.Trace(infoschema.ErrDatabaseNotExists)
	} else if err != nil {
		return errors.Trace(err)
	}

	for _, tbl := range tables {
		if tbl.Name.L == tbInfo.Name.L {
			if tbl.ID != tbInfo.ID {
				// table exists, can't create, we should cancel this job now.
				job.State = model.JobCancelled
				return errors.Trace(infoschema.ErrTableExists)
			}

			tbInfo = tbl
		}
	}

	ver, err := updateSchemaVersion(t, job)
	if err != nil {
		return errors.Trace(err)
	}

	switch tbInfo.State {
	case model.StateNone:
		// none -> public
		job.SchemaState = model.StatePublic
		tbInfo.State = model.StatePublic
		err = t.CreateTable(schemaID, tbInfo)
		if err != nil {
			return errors.Trace(err)
		}
		// finish this job
		job.State = model.JobDone
		addTableHistoryInfo(job, ver, tbInfo)
		return nil
	default:
		return ErrInvalidTableState.Gen("invalid table state %v", tbInfo.State)
	}
}

func (d *ddl) delReorgTable(t *meta.Meta, job *model.Job) error {
	limit := defaultBatchSize
	delCount, err := d.dropTableData(job.TableID, job, limit)
	if err != nil {
		return errors.Trace(err)
	}
	// finish this background job
	if delCount < limit {
		job.SchemaState = model.StateNone
		job.State = model.JobDone
	}
	return nil
}

func (d *ddl) onDropTable(t *meta.Meta, job *model.Job) error {
	schemaID := job.SchemaID
	tableID := job.TableID

	tblInfo, err := t.GetTable(schemaID, tableID)
	if terror.ErrorEqual(err, meta.ErrDBNotExists) {
		job.State = model.JobCancelled
		return errors.Trace(infoschema.ErrDatabaseNotExists)
	} else if err != nil {
		return errors.Trace(err)
	}

	if tblInfo == nil {
		job.State = model.JobCancelled
		return errors.Trace(infoschema.ErrTableNotExists)
	}

	ver, err := updateSchemaVersion(t, job)
	if err != nil {
		return errors.Trace(err)
	}

	switch tblInfo.State {
	case model.StatePublic:
		// public -> write only
		job.SchemaState = model.StateWriteOnly
		tblInfo.State = model.StateWriteOnly
		err = t.UpdateTable(schemaID, tblInfo)
	case model.StateWriteOnly:
		// write only -> delete only
		job.SchemaState = model.StateDeleteOnly
		tblInfo.State = model.StateDeleteOnly
		err = t.UpdateTable(schemaID, tblInfo)
	case model.StateDeleteOnly:
		tblInfo.State = model.StateNone
		err = t.UpdateTable(schemaID, tblInfo)
		if err = t.DropTable(job.SchemaID, job.TableID); err != nil {
			break
		}
		// finish this job
		job.State = model.JobDone
		job.SchemaState = model.StateNone
		addTableHistoryInfo(job, ver, tblInfo)
	default:
		err = ErrInvalidTableState.Gen("invalid table state %v", tblInfo.State)
	}

	return errors.Trace(err)
}

func (d *ddl) getTable(schemaID int64, tblInfo *model.TableInfo) (table.Table, error) {
	alloc := autoid.NewAllocator(d.store, schemaID)
	tbl, err := table.TableFromMeta(alloc, tblInfo)
	return tbl, errors.Trace(err)
}

func (d *ddl) getTableInfo(t *meta.Meta, job *model.Job) (*model.TableInfo, error) {
	schemaID := job.SchemaID
	tableID := job.TableID
	tblInfo, err := t.GetTable(schemaID, tableID)
	if terror.ErrorEqual(err, meta.ErrDBNotExists) {
		job.State = model.JobCancelled
		return nil, errors.Trace(infoschema.ErrDatabaseNotExists)
	} else if err != nil {
		return nil, errors.Trace(err)
	} else if tblInfo == nil {
		job.State = model.JobCancelled
		return nil, errors.Trace(infoschema.ErrTableNotExists)
	}

	if tblInfo.State != model.StatePublic {
		job.State = model.JobCancelled
		return nil, ErrInvalidTableState.Gen("table %s is not in public, but %s", tblInfo.Name.L, tblInfo.State)
	}

	return tblInfo, nil
}

// delKeysWithPrefix deletes data in a limited number. If limit < 0, deletes all data.
func (d *ddl) dropTableData(tID int64, job *model.Job, limit int) (int, error) {
	delCount, err := d.delKeysWithPrefix(tablecodec.EncodeTablePrefix(tID), bgJobFlag, job, limit)
	return delCount, errors.Trace(err)
}

// onTruncateTable delete old table meta, and creates a new table identical to old table except for table ID.
// As all the old data is encoded with old table ID, it can not be accessed any more.
// A background job will be created to delete old data.
func (d *ddl) onTruncateTable(t *meta.Meta, job *model.Job) error {
	schemaID := job.SchemaID
	tableID := job.TableID
	var newTableID int64
	err := job.DecodeArgs(&newTableID)
	if err != nil {
		job.State = model.JobCancelled
		return errors.Trace(err)
	}
	tblInfo, err := t.GetTable(schemaID, tableID)
	if terror.ErrorEqual(err, meta.ErrDBNotExists) {
		job.State = model.JobCancelled
		return errors.Trace(infoschema.ErrDatabaseNotExists)
	} else if err != nil {
		return errors.Trace(err)
	}
	if tblInfo == nil {
		job.State = model.JobCancelled
		return errors.Trace(infoschema.ErrTableNotExists)
	}
	err = t.DropTable(schemaID, tableID)
	if err != nil {
		job.State = model.JobCancelled
		return errors.Trace(err)
	}
	tblInfo.ID = newTableID
	err = t.CreateTable(schemaID, tblInfo)
	if err != nil {
		job.State = model.JobCancelled
		return errors.Trace(err)
	}
	ver, err := updateSchemaVersion(t, job)
	if err != nil {
		return errors.Trace(err)
	}
	job.State = model.JobDone
	addTableHistoryInfo(job, ver, tblInfo)
	return nil
}
