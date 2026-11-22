package store

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/adk/v2/session/database"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openADK opens gorm on dialector, the backend's write connections, and makes it ADK's session service: it applies
// ADK's own migration (gorm's AutoMigrate of ADK's models, which follows them when ADK changes) under lock, which keeps
// replicas that start together from migrating at once, and makes every ADK write run the store's fence.
//
// ADK writes in transactions of its own (session.Service.Create, AppendEvent, Delete), through gorm's create, update
// and delete callbacks. The fence is a callback that runs in each of those, after gorm has begun or joined the
// transaction and before the statement: its error rolls the ADK write back, as it does a write of the store's.
func (s *Store) openADK(ctx context.Context, dialector gorm.Dialector, lock func(ctx context.Context, fn func() error) error) error {
	db, err := gorm.Open(dialector, &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		return fmt.Errorf("open ADK sessions: %w", err)
	}
	db = db.WithContext(ctx)
	sessions, err := database.NewSessionServiceFromDB(db)
	if err != nil {
		return fmt.Errorf("open ADK sessions: %w", err)
	}
	if err := lock(ctx, func() error { return database.AutoMigrate(sessions) }); err != nil {
		return fmt.Errorf("migrate ADK sessions: %w", err)
	}
	if err := s.fenceADK(db); err != nil {
		return fmt.Errorf("fence ADK sessions: %w", err)
	}
	s.sessions = sessions
	return nil
}

// fenceADK registers the fence as a gorm callback of every create, update and delete.
func (s *Store) fenceADK(db *gorm.DB) error {
	check := func(tx *gorm.DB) {
		if s.fence == nil || tx.Error != nil {
			return
		}
		pool, ok := tx.Statement.ConnPool.(sqlDB)
		if !ok {
			_ = tx.AddError(errors.New("an ADK write's connection cannot run the store's fence"))
			return
		}
		if err := s.fence(tx.Statement.Context, s.conn.over(pool)); err != nil {
			_ = tx.AddError(err)
		}
	}
	cb := db.Callback()
	for _, r := range []struct {
		op       string
		register func(string, func(*gorm.DB)) error
	}{
		{"create", cb.Create().Before("gorm:create").After("gorm:begin_transaction").Register},
		{"update", cb.Update().Before("gorm:update").After("gorm:begin_transaction").Register},
		{"delete", cb.Delete().Before("gorm:delete").After("gorm:begin_transaction").Register},
	} {
		if err := r.register("agentkourier:fence", check); err != nil {
			return fmt.Errorf("%s: %w", r.op, err)
		}
	}
	return nil
}
