package app

import (
	"cms/internal/db"
	"cms/internal/db/dbq"
)

// JobDBOrWeb returns the job pool when this process runs workers.
func (a *App) JobDBOrWeb() *db.DB {
	if a.JobDB != nil {
		return a.JobDB
	}
	return a.DB
}

func dbqPrune(keep int32, age string) dbq.PruneRevisionsParams {
	return dbq.PruneRevisionsParams{Keep: keep, MinAge: age}
}
