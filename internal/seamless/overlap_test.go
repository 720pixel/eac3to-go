package seamless

import (
	"bytes"
	"testing"
)

func TestRepeatedFramesWithoutBoundaryRemain(t *testing.T) {
	o, _ := NewOverlap(1280)
	a := ac3Fixture(0)
	for i := 0; i < 100; i++ {
		keep, err := o.Accept(Frame{Bytes: a})
		if !keep || err != nil {
			t.Fatal("deleted nonboundary silence")
		}
	}
}

func TestLeadingSilenceAndOneRemovalLimit(t *testing.T) {
	o, _ := NewOverlap(1280)
	a := ac3Fixture(0)
	if err := o.Boundary(WindowsBoundary(0)); err != nil {
		t.Fatal(err)
	}
	keep, err := o.Accept(Frame{Bytes: a})
	if !keep || err != nil {
		t.Fatal("deleted leading frame")
	}
	_ = o.Boundary(WindowsBoundary(0))
	for i := 0; i < 100; i++ {
		keep, err = o.Accept(Frame{Bytes: a})
		if err != nil || keep != (i != 0) {
			t.Fatalf("removal %d: %v %v", i, keep, err)
		}
	}
}

func TestQueuedBoundaryBudgetAndHistoryOwnership(t *testing.T) {
	o, _ := NewOverlap(1280)
	a, b := ac3Fixture(1), ac3Fixture(2)
	_, _ = o.Accept(Frame{Bytes: a})
	_ = o.Boundary(WindowsBoundary(1280))
	keep, _ := o.Accept(Frame{Bytes: b})
	if !keep {
		t.Fatal("removed unequal queued frame")
	}
	keep, _ = o.Accept(Frame{Bytes: bytes.Clone(b)})
	if keep {
		t.Fatal("missed last-frame match within boundary budget")
	}
	// Accept must not retain caller-owned storage.
	b[len(b)-3] = 3
	_ = o.Boundary(WindowsBoundary(0))
	keep, _ = o.Accept(Frame{Bytes: ac3Fixture(2)})
	if keep {
		t.Fatal("history borrowed caller bytes")
	}
	_ = o.Boundary(WindowsBoundary(0))
	keep, _ = o.Accept(Frame{Bytes: a})
	if !keep {
		t.Fatal("removed unequal frame")
	}
	keep, _ = o.Accept(Frame{Bytes: a})
	if !keep {
		t.Fatal("budget did not disarm")
	}
}

func TestCompositeNeedsCoreAndExtensionMatch(t *testing.T) {
	o, _ := NewOverlap(512)
	a := append(coreFixture(96, 1), extensionFixture(200, false, 1)...)
	b := append(coreFixture(96, 1), extensionFixture(200, false, 2)...)
	_, _ = o.Accept(Frame{Bytes: a})
	_ = o.Boundary(WindowsBoundary(0))
	keep, _ := o.Accept(Frame{Bytes: b})
	if !keep {
		t.Fatal("removed frame whose extension differs")
	}
	_ = o.Boundary(WindowsBoundary(0))
	keep, _ = o.Accept(Frame{Bytes: bytes.Clone(b)})
	if keep {
		t.Fatal("missed complete composite duplicate")
	}
}

func TestExplicitBounds(t *testing.T) {
	o, _ := NewOverlap(1280)
	if err := o.Boundary(OverlapLimit{ByteBudget: -1, MaxRemovals: 1}); err == nil {
		t.Fatal("negative budget accepted")
	}
	if _, err := o.Accept(Frame{Bytes: make([]byte, 1281)}); err == nil {
		t.Fatal("oversized frame accepted")
	}
	if _, err := o.Accept(Frame{}); err == nil {
		t.Fatal("empty frame accepted")
	}
}
