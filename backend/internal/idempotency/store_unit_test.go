package idempotency

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pashagolub/pgxmock/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type unitResult struct {
	ID int `json:"id"`
}

func newMockStore(t *testing.T) (*Store, pgxmock.PgxPoolIface) {
	t.Helper()
	mock, err := pgxmock.NewPool()
	require.NoError(t, err)
	t.Cleanup(func() {
		assert.NoError(t, mock.ExpectationsWereMet())
		mock.Close()
	})
	return NewStore(mock, time.Hour), mock
}

func TestNewStoreDefaultsRetention(t *testing.T) {
	assert.Equal(t, DefaultRetention, NewStore(nil, 0).retention)
	assert.Equal(t, time.Hour, NewStore(nil, time.Hour).retention)
}

func TestStoreDoRejectsNilOut(t *testing.T) {
	store := NewStore(nil, time.Hour)
	_, err := store.Do(context.Background(), 1, "k", "create_feed", nil, nil)
	require.Error(t, err)
}

func TestStoreDoFreshExecution(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO tool_idempotency`).
		WithArgs(int64(1), "k", "create_feed").
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`UPDATE tool_idempotency`).
		WithArgs(int64(1), "k", pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit()

	var out unitResult
	ran := false
	replayed, err := store.Do(context.Background(), 1, "k", "create_feed", &out, func(pgx.Tx) (any, error) {
		ran = true
		return unitResult{ID: 7}, nil
	})
	require.NoError(t, err)
	assert.False(t, replayed)
	assert.True(t, ran)
	assert.Equal(t, 7, out.ID)
}

func TestStoreDoReplaySkipsMutation(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO tool_idempotency`).
		WithArgs(int64(1), "k", "create_feed").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery(`SELECT response, tool`).
		WithArgs(int64(1), "k").
		WillReturnRows(pgxmock.NewRows([]string{"response", "tool"}).
			AddRow([]byte(`{"id":9}`), "create_feed"))
	mock.ExpectRollback()

	var out unitResult
	replayed, err := store.Do(context.Background(), 1, "k", "create_feed", &out, func(pgx.Tx) (any, error) {
		t.Fatal("mutation ran on replay")
		return nil, nil
	})
	require.NoError(t, err)
	assert.True(t, replayed)
	assert.Equal(t, 9, out.ID)
}

func TestStoreDoRejectsCrossToolReplay(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO tool_idempotency`).
		WithArgs(int64(1), "k", "delete_feed").
		WillReturnResult(pgxmock.NewResult("INSERT", 0))
	mock.ExpectQuery(`SELECT response, tool`).
		WithArgs(int64(1), "k").
		WillReturnRows(pgxmock.NewRows([]string{"response", "tool"}).
			AddRow([]byte(`{"id":9}`), "create_feed"))
	mock.ExpectRollback()

	var out unitResult
	_, err := store.Do(context.Background(), 1, "k", "delete_feed", &out, func(pgx.Tx) (any, error) {
		t.Fatal("mutation ran on a cross-tool replay")
		return nil, nil
	})
	require.ErrorIs(t, err, ErrKeyReused)
}

func TestStoreDoMutationErrorRollsBack(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO tool_idempotency`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectRollback()

	var out unitResult
	_, err := store.Do(context.Background(), 1, "k", "create_feed", &out, func(pgx.Tx) (any, error) {
		return nil, errors.New("mutation failed")
	})
	require.Error(t, err)
}

func TestStoreDoInsertError(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO tool_idempotency`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("insert failed"))
	mock.ExpectRollback()

	var out unitResult
	_, err := store.Do(context.Background(), 1, "k", "create_feed", &out, func(pgx.Tx) (any, error) {
		return unitResult{ID: 1}, nil
	})
	require.Error(t, err)
}

func TestStoreDoUpdateErrorRollsBack(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO tool_idempotency`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`UPDATE tool_idempotency`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnError(errors.New("update failed"))
	mock.ExpectRollback()

	var out unitResult
	_, err := store.Do(context.Background(), 1, "k", "create_feed", &out, func(pgx.Tx) (any, error) {
		return unitResult{ID: 1}, nil
	})
	require.Error(t, err)
}

func TestStoreDoCommitError(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO tool_idempotency`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectExec(`UPDATE tool_idempotency`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("UPDATE", 1))
	mock.ExpectCommit().WillReturnError(errors.New("commit failed"))

	var out unitResult
	_, err := store.Do(context.Background(), 1, "k", "create_feed", &out, func(pgx.Tx) (any, error) {
		return unitResult{ID: 1}, nil
	})
	require.Error(t, err)
}

func TestStoreDoUnmarshalableResult(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectBegin()
	mock.ExpectExec(`INSERT INTO tool_idempotency`).
		WithArgs(pgxmock.AnyArg(), pgxmock.AnyArg(), pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("INSERT", 1))
	mock.ExpectRollback()

	var out unitResult
	_, err := store.Do(context.Background(), 1, "k", "create_feed", &out, func(pgx.Tx) (any, error) {
		return make(chan int), nil
	})
	require.Error(t, err)
}

func TestStoreDeleteExpiredUnit(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectExec(`DELETE FROM tool_idempotency`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnResult(pgxmock.NewResult("DELETE", 3))

	deleted, err := store.DeleteExpired(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(3), deleted)
}

func TestStoreDeleteExpiredErrorUnit(t *testing.T) {
	store, mock := newMockStore(t)

	mock.ExpectExec(`DELETE FROM tool_idempotency`).
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("delete failed"))

	_, err := store.DeleteExpired(context.Background())
	require.Error(t, err)
}

func TestStoreRunRetentionStops(t *testing.T) {
	store := NewStore(nil, time.Hour)
	stop := store.RunRetention(time.Hour)
	stop()
}

type execSignalingPool struct {
	ch chan struct{}
}

func (p *execSignalingPool) Exec(context.Context, string, ...interface{}) (pgconn.CommandTag, error) {
	select {
	case p.ch <- struct{}{}:
	default:
	}
	return pgconn.NewCommandTag("DELETE 0"), nil
}

func (p *execSignalingPool) Query(context.Context, string, ...interface{}) (pgx.Rows, error) {
	return nil, errors.New("unused")
}

func (p *execSignalingPool) QueryRow(context.Context, string, ...interface{}) pgx.Row {
	return nil
}

func (p *execSignalingPool) Begin(context.Context) (pgx.Tx, error) {
	return nil, errors.New("unused")
}

func TestStoreRunRetentionSweeps(t *testing.T) {
	ch := make(chan struct{}, 1)
	store := NewStore(&execSignalingPool{ch: ch}, time.Hour)
	stop := store.RunRetention(time.Millisecond)
	defer stop()

	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("retention did not sweep")
	}
}
