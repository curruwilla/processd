package queue

import "testing"

func TestSlots_TryAcquire(t *testing.T) {
	t.Parallel()

	t.Run("honours the global limit", func(t *testing.T) {
		t.Parallel()

		slots := NewSlots(2)

		for i := range 2 {
			if !slots.TryAcquire("invoice") {
				t.Fatalf("TryAcquire() failed on slot %d, want it to succeed", i+1)
			}
		}

		if slots.TryAcquire("other") {
			t.Error("TryAcquire() succeeded past the global limit, want it to fail")
		}
	})

	t.Run("honours the per-worker limit while slots remain", func(t *testing.T) {
		t.Parallel()

		slots := NewSlots(10)
		slots.SetWorkerLimit("invoice", 1)

		if !slots.TryAcquire("invoice") {
			t.Fatal("TryAcquire() failed on the first slot, want it to succeed")
		}

		if slots.TryAcquire("invoice") {
			t.Error("TryAcquire() succeeded past the worker limit, want it to fail")
		}

		if !slots.TryAcquire("other") {
			t.Error("TryAcquire() failed for another worker, want the global room to be usable")
		}
	})

	t.Run("zero worker limit means only the global limit applies", func(t *testing.T) {
		t.Parallel()

		slots := NewSlots(3)
		slots.SetWorkerLimit("invoice", 0)

		for i := range 3 {
			if !slots.TryAcquire("invoice") {
				t.Fatalf("TryAcquire() failed on slot %d, want it to succeed", i+1)
			}
		}

		if slots.TryAcquire("invoice") {
			t.Error("TryAcquire() succeeded past the global limit, want it to fail")
		}
	})

	t.Run("release frees both counters", func(t *testing.T) {
		t.Parallel()

		slots := NewSlots(1)
		slots.SetWorkerLimit("invoice", 1)

		if !slots.TryAcquire("invoice") {
			t.Fatal("TryAcquire() failed, want it to succeed")
		}

		slots.Release("invoice")

		used, limit := slots.Usage()
		if used != 0 || limit != 1 {
			t.Errorf("Usage() = (%d, %d), want (0, 1)", used, limit)
		}

		if !slots.TryAcquire("invoice") {
			t.Error("TryAcquire() failed after Release, want the slot to be free again")
		}
	})
}
