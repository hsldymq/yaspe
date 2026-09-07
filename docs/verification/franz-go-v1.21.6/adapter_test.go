package verify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/twmb/franz-go/pkg/kerr"
	"github.com/twmb/franz-go/pkg/kfake"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/kmsg"
)

// Exercise Kafka wire encoding over net.Pipe, with no external broker or sockets.
type memoryNet struct {
	mu        sync.Mutex
	listeners map[string]*listener
}
type listener struct {
	addr     *net.TCPAddr
	incoming chan net.Conn
	closed   chan struct{}
	once     sync.Once
}

func (l *listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.incoming:
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}
func (l *listener) Close() error   { l.once.Do(func() { close(l.closed) }); return nil }
func (l *listener) Addr() net.Addr { return l.addr }
func (m *memoryNet) listen(_, _ string) (net.Listener, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listeners == nil {
		m.listeners = make(map[string]*listener)
	}
	l := &listener{addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 19000 + len(m.listeners)}, incoming: make(chan net.Conn), closed: make(chan struct{})}
	m.listeners[l.addr.String()] = l
	return l, nil
}
func (m *memoryNet) dial(ctx context.Context, _, address string) (net.Conn, error) {
	m.mu.Lock()
	l := m.listeners[address]
	m.mu.Unlock()
	if l == nil {
		return nil, fmt.Errorf("unknown in-memory broker %s", address)
	}
	client, server := net.Pipe()
	select {
	case l.incoming <- server:
		return client, nil
	case <-ctx.Done():
		client.Close()
		server.Close()
		return nil, ctx.Err()
	case <-l.closed:
		client.Close()
		server.Close()
		return nil, net.ErrClosed
	}
}

type fixture struct {
	cluster *kfake.Cluster
	network *memoryNet
}

func setup(t *testing.T) *fixture {
	t.Helper()
	m := new(memoryNet)
	c, err := kfake.NewCluster(kfake.NumBrokers(1), kfake.SeedTopics(1, "records"), kfake.ListenFn(m.listen))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return &fixture{c, m}
}
func (f *fixture) client(t *testing.T, opts ...kgo.Opt) *kgo.Client {
	t.Helper()
	base := []kgo.Opt{kgo.SeedBrokers(f.cluster.ListenAddrs()...), kgo.Dialer(f.network.dial), kgo.RequestRetries(0), kgo.FetchMaxWait(10 * time.Millisecond)}
	cl, err := kgo.NewClient(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cl.CloseAllowingRebalance)
	return cl
}
func (f *fixture) seed(t *testing.T, n int) {
	t.Helper()
	p := f.client(t, kgo.RecordPartitioner(kgo.ManualPartitioner()), kgo.ProducerBatchCompression(kgo.NoCompression()))
	rs := make([]*kgo.Record, n)
	for i := range rs {
		rs[i] = &kgo.Record{Topic: "records", Value: []byte(fmt.Sprint(i))}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.ProduceSync(ctx, rs...).FirstErr(); err != nil {
		t.Fatal(err)
	}
}
func poll(t *testing.T, cl *kgo.Client, n int) []*kgo.Record {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	fs := cl.PollRecords(ctx, n)
	if err := fs.Err(); err != nil {
		t.Fatal(err)
	}
	return fs.Records()
}
func receive[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for barrier")
		var zero T
		return zero
	}
}
func groupClient(t *testing.T, f *fixture, opts ...kgo.Opt) *kgo.Client {
	base := []kgo.Opt{kgo.ConsumerGroup(t.Name()), kgo.DisableAutoCommit(), kgo.ConsumeTopics("records"), kgo.BlockRebalanceOnPoll(), kgo.Balancers(kgo.CooperativeStickyBalancer())}
	return f.client(t, append(base, opts...)...)
}
func positions(rs []*kgo.Record) map[string]map[int32]kgo.EpochOffset {
	r := rs[len(rs)-1]
	return map[string]map[int32]kgo.EpochOffset{r.Topic: {r.Partition: {Epoch: r.LeaderEpoch, Offset: r.Offset + 1}}}
}

func TestPartialPollRetainsFetchBudgetAndPauseResume(t *testing.T) {
	f := setup(t)
	f.seed(t, 12)
	var requests atomic.Int32
	f.cluster.ControlKey(1, func(kmsg.Request) (kmsg.Response, error, bool) { requests.Add(1); return nil, nil, false })
	cl := f.client(t, kgo.ConsumePartitions(map[string]map[int32]kgo.Offset{"records": {0: kgo.NewOffset().AtStart()}}), kgo.MaxConcurrentFetches(1))
	first := poll(t, cl, 2)
	if len(first) != 2 || cl.BufferedFetchRecords() != 10 {
		t.Fatalf("polled=%d buffered=%d, want 2/10", len(first), cl.BufferedFetchRecords())
	}
	cl.PauseFetchPartitions(map[string][]int32{"records": {0}})
	if cl.BufferedFetchRecords() != 10 {
		t.Fatal("pause without polling changed retained buffer")
	}
	cl.ResumeFetchPartitions(map[string][]int32{"records": {0}})
	all := append([]*kgo.Record(nil), first...)
	for len(all) < 12 {
		if requests.Load() != 1 {
			t.Fatalf("new fetch while previous response still buffered: %d", requests.Load())
		}
		all = append(all, poll(t, cl, 2)...)
	}
	for i, r := range all {
		if r.Offset != int64(i) {
			t.Fatalf("offset[%d]=%d", i, r.Offset)
		}
	}
	t.Log("one fetch delivered as six polls of two; pause/resume retained remaining records")
}

func TestClassicAndRebalanceWindow(t *testing.T) {
	f := setup(t)
	f.seed(t, 3)
	var classic, newProtocol atomic.Int32
	f.cluster.ControlKey(11, func(kmsg.Request) (kmsg.Response, error, bool) { classic.Add(1); return nil, nil, false })
	f.cluster.ControlKey(68, func(kmsg.Request) (kmsg.Response, error, bool) { newProtocol.Add(1); return nil, nil, false })
	blocked := make(chan struct{}, 8)
	revoked := make(chan int, 8)
	cl := groupClient(t, f, kgo.OnPartitionsAssigned(func(context.Context, *kgo.Client, map[string][]int32) {}), kgo.OnPartitionsCallbackBlocked(func(context.Context, *kgo.Client) { blocked <- struct{}{} }), kgo.OnPartitionsRevoked(func(_ context.Context, _ *kgo.Client, ps map[string][]int32) {
		n := 0
		for _, p := range ps {
			n += len(p)
		}
		revoked <- n
	}))
	poll(t, cl, 1)
	cl.ForceRebalance()
	receive(t, blocked)
	select {
	case <-revoked:
		t.Fatal("revoke callback ran inside protected poll window")
	default:
	}
	cl.AllowRebalance()
	n := receive(t, revoked)
	if n != 0 {
		t.Fatalf("unchanged cooperative session revoke has %d partitions", n)
	}
	if classic.Load() == 0 || newProtocol.Load() != 0 {
		t.Fatalf("classic requests=%d next-gen=%d", classic.Load(), newProtocol.Load())
	}
	t.Log("classic JoinGroup observed; revoke waited for AllowRebalance; empty revoke observed")
}

func TestInFlightCommitCancellation(t *testing.T) {
	f := setup(t)
	f.seed(t, 2)
	cl := groupClient(t, f)
	rs := poll(t, cl, 1)
	cl.AllowRebalance()
	entered := make(chan struct{}, 1)
	f.cluster.ControlKey(8, func(kmsg.Request) (kmsg.Response, error, bool) { entered <- struct{}{}; return nil, nil, true })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		cl.CommitOffsetsSync(ctx, positions(rs), func(_ *kgo.Client, _ *kmsg.OffsetCommitRequest, _ *kmsg.OffsetCommitResponse, err error) {
			result <- err
		})
	}()
	receive(t, entered)
	cancel()
	err := receive(t, result)
	receive(t, done)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel returned %v", err)
	}
	t.Log("commit observed on wire; cancellation ended lone synchronous call with context.Canceled")
}

func TestCommitWaitsForCallbackReturn(t *testing.T) {
	f := setup(t)
	f.seed(t, 2)
	cl := groupClient(t, f)
	rs := poll(t, cl, 1)
	cl.AllowRebalance()
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var commitErr error
	go func() {
		defer close(done)
		cl.CommitOffsetsSync(ctx, positions(rs), func(_ *kgo.Client, _ *kmsg.OffsetCommitRequest, resp *kmsg.OffsetCommitResponse, err error) {
			commitErr = err
			if err == nil {
				for _, topic := range resp.Topics {
					for _, p := range topic.Partitions {
						if p.ErrorCode != 0 {
							commitErr = fmt.Errorf("partition error %d", p.ErrorCode)
						}
					}
				}
			}
			close(entered)
			<-release
		})
	}()
	receive(t, entered)
	cancel()
	select {
	case <-done:
		t.Fatal("synchronous commit returned before callback")
	default:
	}
	once.Do(func() { close(release) })
	receive(t, done)
	if commitErr != nil {
		t.Fatal(commitErr)
	}
	t.Log("context cancellation does not bypass a blocked successful commit callback")
}

func TestCloseWaitsForRevokeCallback(t *testing.T) {
	f := setup(t)
	f.seed(t, 2)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	cl := groupClient(t, f, kgo.OnPartitionsRevoked(func(context.Context, *kgo.Client, map[string][]int32) { entered <- struct{}{}; <-release }))
	// Release before client cleanup even if a test assertion fails.
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	poll(t, cl, 1)
	cl.AllowRebalance()
	done := make(chan struct{})
	go func() { cl.Close(); close(done) }()
	receive(t, entered)
	select {
	case <-done:
		t.Fatal("close returned before revoke callback")
	default:
	}
	once.Do(func() { close(release) })
	receive(t, done)
	t.Log("Close waits for revoke callback; callback must not synchronously Close its own client")
}

type sessionEvent struct {
	kind       string
	err        error
	partitions int
}
type groupErrorHook struct{ events chan sessionEvent }

func (h groupErrorHook) OnGroupManageError(err error) {
	h.events <- sessionEvent{kind: "error", err: err}
}
func partitionCount(ps map[string][]int32) int {
	n := 0
	for _, p := range ps {
		n += len(p)
	}
	return n
}

func TestLostThenErrorThenReassignmentWithoutPolling(t *testing.T) {
	f := setup(t)
	f.seed(t, 2)
	events := make(chan sessionEvent, 32)
	cl := groupClient(t, f, kgo.HeartbeatInterval(50*time.Millisecond), kgo.WithHooks(groupErrorHook{events}),
		kgo.OnPartitionsAssigned(func(_ context.Context, _ *kgo.Client, ps map[string][]int32) {
			events <- sessionEvent{kind: "assigned", partitions: partitionCount(ps)}
		}),
		kgo.OnPartitionsLost(func(_ context.Context, _ *kgo.Client, ps map[string][]int32) {
			events <- sessionEvent{kind: "lost", partitions: partitionCount(ps)}
		}))
	poll(t, cl, 1)
	cl.AllowRebalance()
	if e := receive(t, events); e.kind != "assigned" {
		t.Fatalf("initial event: %+v", e)
	}
	cl.PauseFetchPartitions(map[string][]int32{"records": {0}})
	f.cluster.ControlKey(12, func(req kmsg.Request) (kmsg.Response, error, bool) {
		resp := req.ResponseKind().(*kmsg.HeartbeatResponse)
		resp.ErrorCode = kerr.UnknownMemberID.Code
		return resp, nil, true
	})
	lost := receive(t, events)
	failure := receive(t, events)
	assigned := receive(t, events)
	if lost.kind != "lost" || lost.partitions != 1 {
		t.Fatalf("lost event: %+v", lost)
	}
	if failure.kind != "error" || !errors.Is(failure.err, kerr.UnknownMemberID) {
		t.Fatalf("error event: %+v", failure)
	}
	if assigned.kind != "assigned" {
		t.Fatalf("recovery event: %+v", assigned)
	}
	t.Log("with fetch paused and no further poll: Lost -> GroupManageError(UnknownMemberID) -> Assigned")
}

func TestStartupAuthorizationFailureWithoutAnyPoll(t *testing.T) {
	f := setup(t)
	events := make(chan sessionEvent, 32)
	f.cluster.ControlKey(11, func(req kmsg.Request) (kmsg.Response, error, bool) {
		resp := req.ResponseKind().(*kmsg.JoinGroupResponse)
		resp.ErrorCode = kerr.GroupAuthorizationFailed.Code
		return resp, nil, true
	})
	groupClient(t, f, kgo.WithHooks(groupErrorHook{events}), kgo.OnPartitionsLost(func(_ context.Context, _ *kgo.Client, ps map[string][]int32) {
		events <- sessionEvent{kind: "lost", partitions: partitionCount(ps)}
	}))
	lost := receive(t, events)
	failure := receive(t, events)
	if lost.kind != "lost" || lost.partitions != 0 {
		t.Fatalf("startup lost event: %+v", lost)
	}
	if failure.kind != "error" || !errors.Is(failure.err, kerr.GroupAuthorizationFailed) {
		t.Fatalf("startup error event: %+v", failure)
	}
	t.Log("startup group authorization error observable through hook without any business poll; Lost set is empty")
}
