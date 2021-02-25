package submatview

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/stretchr/testify/require"

	"github.com/hashicorp/consul/agent/cache"
	"github.com/hashicorp/consul/lib/ttlcache"
	"github.com/hashicorp/consul/proto/pbcommon"
	"github.com/hashicorp/consul/proto/pbservice"
	"github.com/hashicorp/consul/proto/pbsubscribe"
	"github.com/hashicorp/consul/sdk/testutil/retry"
)

func TestStore_Get(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := NewStore(hclog.New(nil))
	go store.Run(ctx)

	req := &fakeRequest{
		client: NewTestStreamingClient(pbcommon.DefaultEnterpriseMeta.Namespace),
	}
	req.client.QueueEvents(
		newEndOfSnapshotEvent(2),
		newEventServiceHealthRegister(10, 1, "srv1"),
		newEventServiceHealthRegister(22, 2, "srv1"))

	runStep(t, "from empty store, starts materializer", func(t *testing.T) {
		result, err := store.Get(ctx, req)
		require.NoError(t, err)
		require.Equal(t, uint64(22), result.Index)

		r, ok := result.Value.(fakeResult)
		require.True(t, ok)
		require.Len(t, r.srvs, 2)
		require.Equal(t, uint64(22), r.index)

		store.lock.Lock()
		defer store.lock.Unlock()
		require.Len(t, store.byKey, 1)
		e := store.byKey[makeEntryKey(req.Type(), req.CacheInfo())]
		require.Equal(t, 0, e.expiry.Index())
		require.Equal(t, 0, e.requests)

		require.Equal(t, store.expiryHeap.Next().Entry, e.expiry)
	})

	runStep(t, "with an index that already exists in the view", func(t *testing.T) {
		req.index = 21
		result, err := store.Get(ctx, req)
		require.NoError(t, err)
		require.Equal(t, uint64(22), result.Index)

		r, ok := result.Value.(fakeResult)
		require.True(t, ok)
		require.Len(t, r.srvs, 2)
		require.Equal(t, uint64(22), r.index)

		store.lock.Lock()
		defer store.lock.Unlock()
		require.Len(t, store.byKey, 1)
		e := store.byKey[makeEntryKey(req.Type(), req.CacheInfo())]
		require.Equal(t, 0, e.expiry.Index())
		require.Equal(t, 0, e.requests)

		require.Equal(t, store.expiryHeap.Next().Entry, e.expiry)
	})

	runStep(t, "blocks with an index that is not yet in the view", func(t *testing.T) {
		req.index = 23

		chResult := make(chan resultOrError, 1)
		go func() {
			result, err := store.Get(ctx, req)
			chResult <- resultOrError{Result: result, Err: err}
		}()

		select {
		case <-chResult:
			t.Fatalf("expected Get to block")
		case <-time.After(50 * time.Millisecond):
		}

		store.lock.Lock()
		e := store.byKey[makeEntryKey(req.Type(), req.CacheInfo())]
		store.lock.Unlock()
		require.Equal(t, 1, e.requests)

		req.client.QueueEvents(newEventServiceHealthRegister(24, 1, "srv1"))

		var getResult resultOrError
		select {
		case getResult = <-chResult:
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("expected Get to unblock when new events are received")
		}

		require.NoError(t, getResult.Err)
		require.Equal(t, uint64(24), getResult.Result.Index)

		r, ok := getResult.Result.Value.(fakeResult)
		require.True(t, ok)
		require.Len(t, r.srvs, 2)
		require.Equal(t, uint64(24), r.index)

		store.lock.Lock()
		defer store.lock.Unlock()
		require.Len(t, store.byKey, 1)
		e = store.byKey[makeEntryKey(req.Type(), req.CacheInfo())]
		require.Equal(t, 0, e.expiry.Index())
		require.Equal(t, 0, e.requests)

		require.Equal(t, store.expiryHeap.Next().Entry, e.expiry)
	})
}

type resultOrError struct {
	Result Result
	Err    error
}

type fakeRequest struct {
	index  uint64
	key    string
	client *TestStreamingClient
}

func (r *fakeRequest) CacheInfo() cache.RequestInfo {
	key := r.key
	if key == "" {
		key = "key"
	}
	return cache.RequestInfo{
		Key:        key,
		Token:      "abcd",
		Datacenter: "dc1",
		Timeout:    4 * time.Second,
		MinIndex:   r.index,
	}
}

func (r *fakeRequest) NewMaterializer() *Materializer {
	return NewMaterializer(Deps{
		View:   &fakeView{srvs: make(map[string]*pbservice.CheckServiceNode)},
		Client: r.client,
		Logger: hclog.New(nil),
		Request: func(index uint64) pbsubscribe.SubscribeRequest {
			req := pbsubscribe.SubscribeRequest{
				Topic:      pbsubscribe.Topic_ServiceHealth,
				Key:        "key",
				Token:      "abcd",
				Datacenter: "dc1",
				Index:      index,
				Namespace:  pbcommon.DefaultEnterpriseMeta.Namespace,
			}
			return req
		},
	})
}

func (r *fakeRequest) Type() string {
	return fmt.Sprintf("%T", r)
}

type fakeView struct {
	srvs map[string]*pbservice.CheckServiceNode
}

func (f *fakeView) Update(events []*pbsubscribe.Event) error {
	for _, event := range events {
		serviceHealth := event.GetServiceHealth()
		if serviceHealth == nil {
			return fmt.Errorf("unexpected event type for service health view: %T",
				event.GetPayload())
		}

		id := serviceHealth.CheckServiceNode.UniqueID()
		switch serviceHealth.Op {
		case pbsubscribe.CatalogOp_Register:
			f.srvs[id] = serviceHealth.CheckServiceNode

		case pbsubscribe.CatalogOp_Deregister:
			delete(f.srvs, id)
		}
	}
	return nil
}

func (f *fakeView) Result(index uint64) interface{} {
	srvs := make([]*pbservice.CheckServiceNode, 0, len(f.srvs))
	for _, srv := range f.srvs {
		srvs = append(srvs, srv)
	}
	return fakeResult{srvs: srvs, index: index}
}

type fakeResult struct {
	srvs  []*pbservice.CheckServiceNode
	index uint64
}

func (f *fakeView) Reset() {
	f.srvs = make(map[string]*pbservice.CheckServiceNode)
}

func TestStore_Notify(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := NewStore(hclog.New(nil))
	go store.Run(ctx)

	req := &fakeRequest{
		client: NewTestStreamingClient(pbcommon.DefaultEnterpriseMeta.Namespace),
	}
	req.client.QueueEvents(
		newEndOfSnapshotEvent(2),
		newEventServiceHealthRegister(22, 2, "srv1"))

	cID := "correlate"
	ch := make(chan cache.UpdateEvent)

	err := store.Notify(ctx, req, cID, ch)
	require.NoError(t, err)

	runStep(t, "from empty store, starts materializer", func(t *testing.T) {
		store.lock.Lock()
		defer store.lock.Unlock()
		require.Len(t, store.byKey, 1)
		e := store.byKey[makeEntryKey(req.Type(), req.CacheInfo())]
		require.Equal(t, ttlcache.NotIndexed, e.expiry.Index())
		require.Equal(t, 1, e.requests)
	})

	runStep(t, "updates are received", func(t *testing.T) {
		select {
		case update := <-ch:
			require.NoError(t, update.Err)
			require.Equal(t, cID, update.CorrelationID)
			require.Equal(t, uint64(22), update.Meta.Index)
			require.Equal(t, uint64(22), update.Result.(fakeResult).index)
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("expected Get to unblock when new events are received")
		}

		req.client.QueueEvents(newEventServiceHealthRegister(24, 2, "srv1"))

		select {
		case update := <-ch:
			require.NoError(t, update.Err)
			require.Equal(t, cID, update.CorrelationID)
			require.Equal(t, uint64(24), update.Meta.Index)
			require.Equal(t, uint64(24), update.Result.(fakeResult).index)
		case <-time.After(100 * time.Millisecond):
			t.Fatalf("expected Get to unblock when new events are received")
		}
	})

	runStep(t, "closing the notify starts the expiry counter", func(t *testing.T) {
		cancel()

		retry.Run(t, func(r *retry.R) {
			store.lock.Lock()
			defer store.lock.Unlock()
			e := store.byKey[makeEntryKey(req.Type(), req.CacheInfo())]
			require.Equal(r, 0, e.expiry.Index())
			require.Equal(r, 0, e.requests)
			require.Equal(r, store.expiryHeap.Next().Entry, e.expiry)
		})
	})
}

func TestStore_Notify_ManyRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store := NewStore(hclog.New(nil))
	go store.Run(ctx)

	req := &fakeRequest{
		client: NewTestStreamingClient(pbcommon.DefaultEnterpriseMeta.Namespace),
	}
	req.client.QueueEvents(newEndOfSnapshotEvent(2))

	cID := "correlate"
	ch1 := make(chan cache.UpdateEvent)
	ch2 := make(chan cache.UpdateEvent)

	require.NoError(t, store.Notify(ctx, req, cID, ch1))
	assertRequestCount(t, store, req, 1)

	require.NoError(t, store.Notify(ctx, req, cID, ch2))
	assertRequestCount(t, store, req, 2)

	req.index = 15

	go func() {
		_, _ = store.Get(ctx, req)
	}()

	retry.Run(t, func(r *retry.R) {
		assertRequestCount(r, store, req, 3)
	})

	go func() {
		_, _ = store.Get(ctx, req)
	}()

	retry.Run(t, func(r *retry.R) {
		assertRequestCount(r, store, req, 4)
	})

	var req2 *fakeRequest

	runStep(t, "Get and Notify with a different key", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		req2 = &fakeRequest{client: req.client, key: "key2"}

		require.NoError(t, store.Notify(ctx, req2, cID, ch1))
		go func() {
			_, _ = store.Get(ctx, req2)
		}()

		// the original entry should still be at count 4
		assertRequestCount(t, store, req, 4)
		// the new entry should be at count 2
		retry.Run(t, func(r *retry.R) {
			assertRequestCount(r, store, req2, 2)
		})
	})

	runStep(t, "end all the requests", func(t *testing.T) {
		req.client.QueueEvents(
			newEventServiceHealthRegister(10, 1, "srv1"),
			newEventServiceHealthRegister(12, 2, "srv1"),
			newEventServiceHealthRegister(13, 1, "srv2"),
			newEventServiceHealthRegister(16, 3, "srv2"))

		// The two Get requests should exit now that the index has been updated
		retry.Run(t, func(r *retry.R) {
			assertRequestCount(r, store, req, 2)
		})

		// Cancel the context so all requests terminate
		cancel()
		retry.Run(t, func(r *retry.R) {
			assertRequestCount(r, store, req, 0)
		})
	})

	runStep(t, "the expiry heap should contain two entries", func(t *testing.T) {
		store.lock.Lock()
		defer store.lock.Unlock()
		e := store.byKey[makeEntryKey(req.Type(), req.CacheInfo())]
		e2 := store.byKey[makeEntryKey(req2.Type(), req2.CacheInfo())]
		require.Equal(t, 0, e2.expiry.Index())
		require.Equal(t, 1, e.expiry.Index())

		require.Equal(t, store.expiryHeap.Next().Entry, e2.expiry)
	})
}

type testingT interface {
	Helper()
	Fatalf(string, ...interface{})
}

func assertRequestCount(t testingT, s *Store, req Request, expected int) {
	t.Helper()

	key := makeEntryKey(req.Type(), req.CacheInfo())

	s.lock.Lock()
	defer s.lock.Unlock()
	actual := s.byKey[key].requests
	if actual != expected {
		t.Fatalf("expected request count to be %d, got %d", expected, actual)
	}
}

func TestStore_Run_ExpiresEntries(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ttl := 10 * time.Millisecond
	patchIdleTTL(t, ttl)

	store := NewStore(hclog.New(nil))
	go store.Run(ctx)

	req := &fakeRequest{
		client: NewTestStreamingClient(pbcommon.DefaultEnterpriseMeta.Namespace),
	}
	req.client.QueueEvents(newEndOfSnapshotEvent(2))

	cID := "correlate"
	ch1 := make(chan cache.UpdateEvent)

	reqCtx, reqCancel := context.WithCancel(context.Background())
	defer reqCancel()

	require.NoError(t, store.Notify(reqCtx, req, cID, ch1))
	assertRequestCount(t, store, req, 1)

	// Get a copy of the entry so that we can check it was expired later
	store.lock.Lock()
	e := store.byKey[makeEntryKey(req.Type(), req.CacheInfo())]
	store.lock.Unlock()

	reqCancel()
	retry.Run(t, func(r *retry.R) {
		assertRequestCount(r, store, req, 0)
	})

	// wait for the entry to expire, with lots of buffer
	time.Sleep(3 * ttl)

	store.lock.Lock()
	defer store.lock.Unlock()
	require.Len(t, store.byKey, 0)
	require.Equal(t, ttlcache.NotIndexed, e.expiry.Index())
}

func patchIdleTTL(t *testing.T, ttl time.Duration) {
	orig := idleTTL
	idleTTL = ttl
	t.Cleanup(func() {
		idleTTL = orig
	})
}

func runStep(t *testing.T, name string, fn func(t *testing.T)) {
	t.Helper()
	if !t.Run(name, fn) {
		t.FailNow()
	}
}
