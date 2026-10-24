package state

import (
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
)

func TestSubscriptionRefreshPersistenceAndMigrationRollback(t *testing.T) {
	repo := newTestStateRepo(t)
	cache := newTestCacheRepo(t)
	sub := model.Subscription{ID: "test", Name: "test", URL: "https://example.com", Enabled: true, UpdateIntervalNs: int64(time.Hour), DownloadMode: "external_proxy"}
	if err := repo.UpsertSubscription(sub); err != nil {
		t.Fatal(err)
	}
	loaded, err := repo.ListSubscriptions()
	if err != nil || len(loaded) != 1 || loaded[0].DownloadMode != "external_proxy" {
		t.Fatal("download mode not persisted", err)
	}
	engine := newStateEngine(repo, cache)
	engine.MarkSubscriptionRefresh("test")
	row := &model.SubscriptionRefresh{SubscriptionID: "test", StateJSON: `{"last_updated_ns":123,"consecutive_failures":2}`}
	if err := engine.FlushDirtySets(CacheReaders{ReadSubscriptionRefresh: func(string) *model.SubscriptionRefresh { return row }}); err != nil {
		t.Fatal(err)
	}
	history, err := cache.LoadSubscriptionRefresh()
	if err != nil || len(history) != 1 || history[0].StateJSON != row.StateJSON {
		t.Fatal("history not restored", err)
	}
	for _, step := range []struct{ kind, path string }{
		{"state", "migrations/state/000008_subscriptions_download.down.sql"},
		{"cache", "migrations/cache/000003_subscription_refresh.down.sql"},
	} {
		data, err := migrationsFS.ReadFile(step.path)
		if err != nil {
			t.Fatal(err)
		}
		db := repo.db
		version := 7
		if step.kind == "cache" {
			db = cache.db
			version = 2
		}
		if _, err := db.Exec(string(data)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("UPDATE schema_migrations SET version=?,dirty=0", version); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	if err := repo.db.QueryRow("SELECT COUNT(*) FROM subscriptions").Scan(&count); err != nil || count != 1 {
		t.Fatal("rollback changed subscriptions", err)
	}
	if err := MigrateStateDB(repo.db); err != nil {
		t.Fatal(err)
	}
	if err := MigrateCacheDB(cache.db); err != nil {
		t.Fatal(err)
	}
	loaded, err = repo.ListSubscriptions()
	if err != nil || loaded[0].DownloadMode != "auto" {
		t.Fatal("upgrade after rollback failed", err)
	}
}
