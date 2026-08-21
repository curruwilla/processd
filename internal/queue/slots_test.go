package queue

import "testing"

func TestSlots_TryAcquire(t *testing.T) {
	t.Parallel()

	t.Run("honours the global limit", func(t *testing.T) {
		t.Parallel()

		slots := NewSlots(2)

		for i := range 2 {
			if !slots.TryAcquire(processID(i), "invoice") {
				t.Fatalf("TryAcquire() failed on slot %d, want it to succeed", i+1)
			}
		}

		if slots.TryAcquire("p-other", "other") {
			t.Error("TryAcquire() succeeded past the global limit, want it to fail")
		}
	})

	t.Run("honours the per-worker limit while slots remain", func(t *testing.T) {
		t.Parallel()

		slots := NewSlots(10)
		slots.SetWorkerLimit("invoice", 1)

		if !slots.TryAcquire("p-1", "invoice") {
			t.Fatal("TryAcquire() failed on the first slot, want it to succeed")
		}

		if slots.TryAcquire("p-2", "invoice") {
			t.Error("TryAcquire() succeeded past the worker limit, want it to fail")
		}

		if !slots.TryAcquire("p-3", "other") {
			t.Error("TryAcquire() failed for another worker, want the global room to be usable")
		}
	})

	t.Run("zero worker limit means only the global limit applies", func(t *testing.T) {
		t.Parallel()

		slots := NewSlots(3)
		slots.SetWorkerLimit("invoice", 0)

		for i := range 3 {
			if !slots.TryAcquire(processID(i), "invoice") {
				t.Fatalf("TryAcquire() failed on slot %d, want it to succeed", i+1)
			}
		}

		if slots.TryAcquire("p-other", "invoice") {
			t.Error("TryAcquire() succeeded past the global limit, want it to fail")
		}
	})

	t.Run("release frees both counters", func(t *testing.T) {
		t.Parallel()

		slots := NewSlots(1)
		slots.SetWorkerLimit("invoice", 1)

		if !slots.TryAcquire("p-1", "invoice") {
			t.Fatal("TryAcquire() failed, want it to succeed")
		}

		slots.Release("p-1")

		used, limit := slots.Usage()
		if used != 0 || limit != 1 {
			t.Errorf("Usage() = (%d, %d), want (0, 1)", used, limit)
		}

		if !slots.TryAcquire("p-2", "invoice") {
			t.Error("TryAcquire() failed after Release, want the slot to be free again")
		}
	})

	// A service keeps its slot across a restart, so the dispatch pass that starts
	// its next attempt acquires a slot it already holds.
	t.Run("re-acquiring by the same execution consumes one slot", func(t *testing.T) {
		t.Parallel()

		slots := NewSlots(1)

		for i := range 3 {
			if !slots.TryAcquire("p-1", "invoice") {
				t.Fatalf("TryAcquire() failed on call %d, want the held slot to be kept", i+1)
			}
		}

		used, _ := slots.Usage()
		if used != 1 {
			t.Errorf("Usage() used = %d, want 1", used)
		}

		if !slots.Holds("p-1") {
			t.Error("Holds() = false, want the execution to hold its slot")
		}
	})

	// launch releases what it reserved on failure, and the supervisor reports the
	// outcome of the same attempt independently. Neither may free a second slot.
	t.Run("releasing twice frees one slot", func(t *testing.T) {
		t.Parallel()

		slots := NewSlots(2)

		if !slots.TryAcquire("p-1", "invoice") {
			t.Fatal("TryAcquire() failed, want it to succeed")
		}

		if !slots.TryAcquire("p-2", "invoice") {
			t.Fatal("TryAcquire() failed, want it to succeed")
		}

		slots.Release("p-1")
		slots.Release("p-1")

		used, _ := slots.Usage()
		if used != 1 {
			t.Errorf("Usage() used = %d, want 1", used)
		}

		if slots.Holds("p-1") {
			t.Error("Holds() = true after Release, want the slot to be gone")
		}
	})

	t.Run("releasing an unknown execution is a no-op", func(t *testing.T) {
		t.Parallel()

		slots := NewSlots(1)

		if !slots.TryAcquire("p-1", "invoice") {
			t.Fatal("TryAcquire() failed, want it to succeed")
		}

		slots.Release("p-unknown")

		used, _ := slots.Usage()
		if used != 1 {
			t.Errorf("Usage() used = %d, want 1", used)
		}
	})
}

func processID(i int) string {
	return string(rune('a' + i))
}
