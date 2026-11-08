package instance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/deadline"
)

// ctxServerClient wraps mockServerClient to record the deadline each call saw
// and, when hang is set, to block until the call's context is done -- the shape
// of a request that never completes.
type ctxServerClient struct {
	*mockServerClient
	hang      bool
	createDL  time.Time
	getDL     time.Time
	listDL    time.Time
	deleteDL  time.Time
	createCtx context.Context
}

func deadlineOf(ctx context.Context) time.Time {
	dl, _ := ctx.Deadline()
	return dl
}

func (c *ctxServerClient) Create(ctx context.Context, opts hcloud.ServerCreateOpts) (hcloud.ServerCreateResult, *hcloud.Response, error) {
	c.createDL, c.createCtx = deadlineOf(ctx), ctx
	if c.hang {
		<-ctx.Done()
		return hcloud.ServerCreateResult{}, nil, ctx.Err()
	}
	return c.mockServerClient.Create(ctx, opts)
}

func (c *ctxServerClient) GetByID(ctx context.Context, id int64) (*hcloud.Server, *hcloud.Response, error) {
	c.getDL = deadlineOf(ctx)
	if c.hang {
		<-ctx.Done()
		return nil, nil, ctx.Err()
	}
	return c.mockServerClient.GetByID(ctx, id)
}

func (c *ctxServerClient) AllWithOpts(ctx context.Context, opts hcloud.ServerListOpts) ([]*hcloud.Server, error) {
	c.listDL = deadlineOf(ctx)
	if c.hang {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return c.mockServerClient.AllWithOpts(ctx, opts)
}

func (c *ctxServerClient) DeleteWithResult(ctx context.Context, s *hcloud.Server) (*hcloud.ServerDeleteResult, *hcloud.Response, error) {
	c.deleteDL = deadlineOf(ctx)
	return c.mockServerClient.DeleteWithResult(ctx, s)
}

// funcWaiter runs fn as WaitFor.
type funcWaiter func(ctx context.Context) error

func (f funcWaiter) WaitFor(ctx context.Context, _ ...*hcloud.Action) error { return f(ctx) }

// within asserts dl is set and lies within want of now, give or take slack.
func within(t *testing.T, name string, dl time.Time, want time.Duration) {
	t.Helper()
	if dl.IsZero() {
		t.Fatalf("%s: call ran with no deadline", name)
	}
	got := time.Until(dl)
	if got > want || got < want-5*time.Second {
		t.Errorf("%s: deadline %s away, want about %s", name, got.Round(time.Second), want)
	}
}

func TestBudgets_EachCallClassGetsItsOwnDeadline(t *testing.T) {
	c := &ctxServerClient{mockServerClient: newMockServerClient()}
	c.action = &hcloud.Action{ID: 1}
	var waitDL time.Time
	p := NewProviderWithWaiter(c, "test-cluster", funcWaiter(func(ctx context.Context) error {
		waitDL = deadlineOf(ctx)
		return nil
	}))

	s, err := p.Create(context.Background(), CreateOpts{Name: "n", ServerType: "cx22", Location: "nbg1"})
	if err != nil {
		t.Fatal(err)
	}
	within(t, "create submission", c.createDL, deadline.CreateSubmit)
	within(t, "boot wait", waitDL, deadline.BootWait)

	if _, err := p.Get(context.Background(), FormatProviderID(s.ID)); err != nil {
		t.Fatal(err)
	}
	within(t, "get", c.getDL, deadline.Read)

	if _, err := p.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	within(t, "list", c.listDL, deadline.Read)

	if err := p.Delete(context.Background(), FormatProviderID(s.ID)); err != nil {
		t.Fatal(err)
	}
	within(t, "delete", c.deleteDL, deadline.Delete)
}

// The submission budget must not reach the boot wait: once hcloud has accepted
// the create the server exists, and cancelling the wait on the submission
// deadline is exactly the path that orphans it (#61).
func TestCreate_SubmissionBudgetDoesNotCancelBootWait(t *testing.T) {
	c := &ctxServerClient{mockServerClient: newMockServerClient()}
	c.action = &hcloud.Action{ID: 1}
	p := NewProviderWithWaiter(c, "test-cluster", funcWaiter(func(ctx context.Context) error {
		// Outlive the submission budget several times over.
		select {
		case <-time.After(150 * time.Millisecond):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})).WithBudgets(deadline.Budgets{Read: time.Second, Delete: time.Second, CreateSubmit: 20 * time.Millisecond, BootWait: 5 * time.Second})

	if _, err := p.Create(context.Background(), CreateOpts{Name: "n", ServerType: "cx22", Location: "nbg1"}); err != nil {
		t.Fatalf("boot wait was cut short by the submission budget: %v", err)
	}
	if c.createCtx.Err() == nil {
		t.Error("submission context still live after the create returned; it should be released")
	}
}

// A cancelled caller context still ends the boot wait: the wait is detached from
// the submission budget, not from the caller.
func TestCreate_CallerCancellationStillEndsBootWait(t *testing.T) {
	c := &ctxServerClient{mockServerClient: newMockServerClient()}
	c.action = &hcloud.Action{ID: 1}
	ctx, cancel := context.WithCancel(context.Background())
	p := NewProviderWithWaiter(c, "test-cluster", funcWaiter(func(ctx context.Context) error {
		cancel()
		<-ctx.Done()
		return ctx.Err()
	}))
	_, err := p.Create(ctx, CreateOpts{Name: "n", ServerType: "cx22", Location: "nbg1"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestBudgets_HungCallsReturnOnTheirBudget(t *testing.T) {
	small := deadline.Budgets{Read: 50 * time.Millisecond, Delete: 50 * time.Millisecond,
		CreateSubmit: 50 * time.Millisecond, BootWait: 50 * time.Millisecond}

	run := func(name string, fn func() error) {
		t.Helper()
		done := make(chan error, 1)
		start := time.Now()
		go func() { done <- fn() }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s: hung call returned nil error", name)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("%s: err = %v, want a deadline error", name, err)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("%s: returned after %s, not on its 50ms budget", name, d)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: hung call never returned; it has no deadline", name)
		}
	}

	hung := &ctxServerClient{mockServerClient: newMockServerClient(), hang: true}
	p := NewProvider(hung, "test-cluster").WithBudgets(small)
	run("create submission", func() error {
		_, err := p.Create(context.Background(), CreateOpts{Name: "n", ServerType: "cx22", Location: "nbg1"})
		return err
	})
	run("get", func() error { _, err := p.Get(context.Background(), FormatProviderID(1)); return err })
	run("list", func() error { _, err := p.List(context.Background()); return err })
	run("delete", func() error { return p.Delete(context.Background(), FormatProviderID(1)) })

	booting := &ctxServerClient{mockServerClient: newMockServerClient()}
	booting.action = &hcloud.Action{ID: 1}
	pw := NewProviderWithWaiter(booting, "test-cluster", funcWaiter(func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})).WithBudgets(small)
	run("boot wait", func() error {
		_, err := pw.Create(context.Background(), CreateOpts{Name: "n", ServerType: "cx22", Location: "nbg1"})
		return err
	})
}
