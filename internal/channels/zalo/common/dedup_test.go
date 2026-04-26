package common

import (
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDedup_FirstAddNotSeen(t *testing.T) {
	d := NewDedup(time.Minute, 100)
	id := uuid.New()
	if d.SeenOrAdd(id, "m1") {
		t.Error("first SeenOrAdd should report not-seen")
	}
}

func TestDedup_DuplicateWithinTTLSeen(t *testing.T) {
	d := NewDedup(time.Minute, 100)
	id := uuid.New()
	d.SeenOrAdd(id, "m1")
	if !d.SeenOrAdd(id, "m1") {
		t.Error("second SeenOrAdd within TTL should report seen")
	}
}

func TestDedup_ExpiryRecyclesEntry(t *testing.T) {
	d := NewDedup(10*time.Millisecond, 100)
	id := uuid.New()
	d.SeenOrAdd(id, "m1")
	time.Sleep(20 * time.Millisecond)
	if d.SeenOrAdd(id, "m1") {
		t.Error("entry should be expired and treated as not-seen")
	}
}

func TestDedup_InstanceScopeIsolation(t *testing.T) {
	d := NewDedup(time.Minute, 100)
	a, b := uuid.New(), uuid.New()
	d.SeenOrAdd(a, "m1")
	if d.SeenOrAdd(b, "m1") {
		t.Error("same messageID under different instanceID should not collide")
	}
}

func TestDedup_MaxCapEvictsOldest(t *testing.T) {
	d := NewDedup(time.Minute, 3)
	id := uuid.New()
	d.SeenOrAdd(id, "m1")
	time.Sleep(time.Millisecond)
	d.SeenOrAdd(id, "m2")
	time.Sleep(time.Millisecond)
	d.SeenOrAdd(id, "m3")
	d.SeenOrAdd(id, "m4") // forces eviction of m1
	if d.Len() != 3 {
		t.Errorf("len = %d, want 3", d.Len())
	}
	if d.SeenOrAdd(id, "m1") {
		t.Error("m1 should have been evicted as oldest")
	}
}

func TestDedup_EmptyMessageIDNotRecorded(t *testing.T) {
	d := NewDedup(time.Minute, 100)
	id := uuid.New()
	if d.SeenOrAdd(id, "") {
		t.Error("empty messageID should never report seen")
	}
	if d.Len() != 0 {
		t.Error("empty messageID should not be recorded")
	}
}

func TestDedup_ConcurrentAccessRaceClean(t *testing.T) {
	d := NewDedup(time.Minute, 1000)
	id := uuid.New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			d.SeenOrAdd(id, "m1")
		}(i)
	}
	wg.Wait()
}
