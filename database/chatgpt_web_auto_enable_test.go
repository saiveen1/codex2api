package database

import (
	"reflect"
	"sync"
	"testing"
)

func TestChatGPTWebAutoInitPreservesStateAndManualDisable(t *testing.T) {
	db, _, id := newChatGPTWebTestDB(t)
	ctx := t.Context()
	first, err := db.InitChatGPTWebAccountEnabled(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Enabled || first.Status != "unverified" || first.ObservedAt != nil || first.EnvironmentRevision != 1 || first.DeviceID == "" || first.SessionID == "" {
		t.Fatalf("invalid initialized state: %+v", first)
	}
	second, err := db.InitChatGPTWebAccountEnabled(ctx, id)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("non-idempotent init: %+v %v", second, err)
	}
	// Both operations serialize on the same owner; whichever starts first,
	// an initializer must never undo the administrator's explicit disable.
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); <-start; _, err := db.InitChatGPTWebAccountEnabled(ctx, id); errs <- err }()
	go func() {
		defer wg.Done()
		<-start
		_, err := db.SetChatGPTWebAccountEnabled(ctx, id, false)
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	disabled, err := db.GetChatGPTWebAccountState(ctx, id)
	if err != nil || disabled.Enabled {
		t.Fatalf("manual disable lost: %+v %v", disabled, err)
	}
	after, err := db.InitChatGPTWebAccountEnabled(ctx, id)
	if err != nil || !reflect.DeepEqual(disabled, after) {
		t.Fatalf("disabled state changed: %+v %v", after, err)
	}
}
