package server

import "testing"

func TestSessionCreatesStableWindowIDsAndSwitches(t *testing.T) {
	s := NewSession(0)
	client := s.NewClient(0)
	_ = client

	pane0 := &Pane{ID: s.AddPaneID(), Title: "bash"}
	w0, c0 := s.CreateWindow(pane0, 0)
	if w0.ID != 0 || c0.ActiveWindowID != 0 || c0.FocusedPaneID != pane0.ID {
		t.Fatalf("first window = %#v client=%#v", w0, c0)
	}

	pane1 := &Pane{ID: s.AddPaneID(), Title: "logs"}
	w1, c1 := s.CreateWindow(pane1, 0)
	if w1.ID != 1 || c1.ActiveWindowID != 1 || c1.FocusedPaneID != pane1.ID {
		t.Fatalf("second window = %#v client=%#v", w1, c1)
	}

	_, _, c2, err := s.SelectWindow(0, 0)
	if err != nil {
		t.Fatalf("SelectWindow() error = %v", err)
	}
	if c2.ActiveWindowID != 0 || c2.FocusedPaneID != pane0.ID {
		t.Fatalf("selected client = %#v", c2)
	}
}

func TestWindowListAndCloseFinalWindowReplacementPolicy(t *testing.T) {
	s := NewSession(0)
	s.NewClient(0)
	pane0 := &Pane{ID: s.AddPaneID(), Title: "bash"}
	s.CreateWindow(pane0, 0)
	list := s.WindowList(0)
	if len(list.Windows) != 1 || list.Windows[0].Index != 0 || !list.Windows[0].Active {
		t.Fatalf("WindowList() = %#v", list)
	}

	closed, closedPane, replacement, pane, client, autoCreate, err := s.CloseWindow(0, 0)
	if err != nil {
		t.Fatalf("CloseWindow() error = %v", err)
	}
	if closed != 0 || closedPane == nil || !autoCreate || replacement != nil || pane != nil || client == nil {
		t.Fatalf("CloseWindow() = %d %#v %#v %#v %#v %v", closed, closedPane, replacement, pane, client, autoCreate)
	}
}

func TestEnsureClientAndReattachReuseExistingSessionState(t *testing.T) {
	s := NewSession(0)
	if client := s.EnsureClient(0); client == nil || client.ActiveWindowID != 0 || client.FocusedPaneID != 0 {
		t.Fatalf("EnsureClient() before windows = %#v", client)
	}

	pane0 := &Pane{ID: s.AddPaneID(), Title: "bash"}
	w0, _ := s.CreateWindow(pane0, 0)
	pane1 := &Pane{ID: s.AddPaneID(), Title: "logs"}
	w1, _ := s.CreateWindow(pane1, 0)

	_, _, selected, err := s.SelectWindow(0, w0.ID)
	if err != nil {
		t.Fatalf("SelectWindow() error = %v", err)
	}
	if selected.ActiveWindowID != w0.ID || selected.FocusedPaneID != pane0.ID {
		t.Fatalf("selected client = %#v", selected)
	}

	reattachWindow, reattachPane, reattachClient, err := s.ReattachClient(0)
	if err != nil {
		t.Fatalf("ReattachClient() error = %v", err)
	}
	if reattachWindow.ID != w0.ID || reattachPane.ID != pane0.ID {
		t.Fatalf("ReattachClient() rebound to wrong target: window=%#v pane=%#v", reattachWindow, reattachPane)
	}
	if reattachClient.BindingGeneration <= selected.BindingGeneration {
		t.Fatalf("BindingGeneration did not advance on reattach: selected=%#v reattach=%#v", selected, reattachClient)
	}

	client1 := s.EnsureClient(1)
	if client1.ActiveWindowID != w0.ID || client1.FocusedPaneID != pane0.ID {
		t.Fatalf("EnsureClient(1) = %#v, want first window %d/%d", client1, w0.ID, pane0.ID)
	}
	if w1.ID != 1 {
		t.Fatalf("second window ID changed unexpectedly: %#v", w1)
	}
}
