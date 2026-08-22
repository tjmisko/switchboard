package codex

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/tjmisko/switchboard/internal/agentgraph"
	"github.com/tjmisko/switchboard/internal/provider"
)

// EndpointDiagnostic is deliberately content-free: raw app-server errors and
// payloads never cross the diagnostics boundary.
type EndpointDiagnostic struct {
	SlotID            string
	Connected         bool
	ThreadID          string
	Generation        uint64
	SnapshotAt        time.Time
	Category          string
	LastErrorCategory string
}

type managedSlotObserver struct {
	endpoint    string
	generation  uint64
	observer    *Observer
	cancelFanIn context.CancelFunc
}

// SlotObserver owns one independent app-server observer per launcher slot.
// Existing Codex processes without an endpoint remain hook-only.
type SlotObserver struct {
	config Config
	queue  *provider.InvalidationQueue

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	slots    map[provider.RootKey]*managedSlotObserver
	bindings *BindingRegistry
	closed   bool
	once     sync.Once
	wg       sync.WaitGroup
}

var _ provider.Observer = (*SlotObserver)(nil)

// NewSlotObserver is the production Codex observation entry point. Config's
// EndpointConnector is a test seam; production launches `app-server proxy
// --sock` for the registered endpoint.
func NewSlotObserver(config Config) *SlotObserver {
	ctx, cancel := context.WithCancel(context.Background())
	return &SlotObserver{
		config: config, queue: provider.NewInvalidationQueue(config.UpdateBuffer),
		ctx: ctx, cancel: cancel, slots: make(map[provider.RootKey]*managedSlotObserver),
		bindings: newBindingRegistry(config.Environment),
	}
}

func (o *SlotObserver) connector(endpoint string) Connector {
	if o.config.EndpointConnector != nil {
		return o.config.EndpointConnector(endpoint)
	}
	return CommandConnector{Sock: endpointSocketPath(endpoint)}
}

func endpointSocketPath(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if strings.HasPrefix(endpoint, "unix://") {
		return strings.TrimPrefix(endpoint, "unix://")
	}
	return endpoint
}

func (o *SlotObserver) ensure(ref provider.RootRef) (*managedSlotObserver, error) {
	if ref.SlotID == "" || ref.ProviderEndpoint == "" {
		return nil, errors.New("codex endpoint unavailable")
	}
	key := ref.Key()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return nil, errors.New("codex slot observer is closed")
	}
	if existing := o.slots[key]; existing != nil && existing.endpoint == ref.ProviderEndpoint {
		if ref.BindingGeneration != 0 {
			existing.generation = ref.BindingGeneration
		}
		return existing, nil
	}
	if existing := o.slots[key]; existing != nil {
		existing.cancelFanIn()
		_ = existing.observer.Close()
	}
	config := o.config
	config.Connector = o.connector(ref.ProviderEndpoint)
	child := NewObserver(config)
	ctx, cancel := context.WithCancel(o.ctx)
	managed := &managedSlotObserver{endpoint: ref.ProviderEndpoint, generation: ref.BindingGeneration, observer: child, cancelFanIn: cancel}
	o.slots[key] = managed
	if binding, _ := o.bindings.resolve(context.Background(), ref); binding.ThreadID != "" {
		_, _ = child.ReconcileHookBinding(key, binding.ThreadID)
	}
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		for {
			select {
			case <-ctx.Done():
				return
			case update := <-child.Updates():
				o.queue.Signal(update)
			}
		}
	}()
	return managed, nil
}

func (o *SlotObserver) Observe(ctx context.Context, ref provider.RootRef, now time.Time) (agentgraph.Observation, error) {
	if ref.SlotID == "" || ref.ProviderEndpoint == "" {
		return agentgraph.Observation{Provider: agentgraph.ProviderCodex, Complete: false, Diagnostic: "slot alive but no endpoint registered"}, nil
	}
	managed, err := o.ensure(ref)
	if err != nil {
		return agentgraph.Observation{Provider: agentgraph.ProviderCodex, Complete: false, Diagnostic: "endpoint disconnected"}, nil
	}
	if ref.ProviderSessionID != "" {
		update, bindErr := o.ReconcileHookBinding(ref.Key(), ref.ProviderSessionID)
		if bindErr != nil {
			return agentgraph.Observation{}, bindErr
		}
		if update.Stale {
			return agentgraph.Observation{Provider: agentgraph.ProviderCodex, Complete: false, Diagnostic: "stale observation rejected"}, nil
		}
	}
	return managed.observer.Observe(ctx, ref, now)
}

func (o *SlotObserver) RegisterHookBinding(key provider.RootKey, threadID string) error {
	_, err := o.ReconcileHookBinding(key, threadID)
	return err
}

func (o *SlotObserver) ReconcileHookBinding(key provider.RootKey, threadID string) (BindingUpdate, error) {
	update, err := o.bindings.RegisterHook(key, threadID)
	if err != nil || update.Stale {
		return update, err
	}
	o.mu.Lock()
	managed := o.slots[key]
	if managed != nil {
		managed.generation = update.Generation
	}
	o.mu.Unlock()
	if managed != nil {
		_, err = managed.observer.ReconcileHookBinding(key, threadID)
	}
	return update, err
}

func (o *SlotObserver) SetThreadName(ctx context.Context, key provider.RootKey, threadID string, generation uint64, name string) error {
	o.mu.Lock()
	managed := o.slots[key]
	valid := managed != nil && (generation == 0 || managed.generation == 0 || managed.generation == generation)
	o.mu.Unlock()
	if !valid {
		return errors.New("codex: stale name generation")
	}
	return managed.observer.SetThreadName(ctx, key, threadID, name)
}

func (o *SlotObserver) Diagnostics() []EndpointDiagnostic {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]EndpointDiagnostic, 0, len(o.slots))
	for key, managed := range o.slots {
		managed.observer.mu.Lock()
		diagnostic := EndpointDiagnostic{SlotID: key.SlotID, Connected: managed.observer.connected, Generation: managed.generation}
		if record := managed.observer.roots[key]; record != nil {
			diagnostic.ThreadID = record.threadID
			diagnostic.SnapshotAt = record.observation.ObservedAt
		}
		diagnostic.LastErrorCategory = managed.observer.lastError
		managed.observer.mu.Unlock()
		if !diagnostic.Connected {
			diagnostic.Category = "endpoint disconnected"
		} else if diagnostic.ThreadID == "" {
			diagnostic.Category = "slot alive but no thread bound"
		}
		out = append(out, diagnostic)
	}
	return out
}

func (o *SlotObserver) Updates() <-chan provider.RootKey { return o.queue.Updates() }

func (o *SlotObserver) Forget(key provider.RootKey) {
	o.bindings.Forget(key)
	o.mu.Lock()
	managed := o.slots[key]
	delete(o.slots, key)
	o.mu.Unlock()
	if managed != nil {
		managed.cancelFanIn()
		_ = managed.observer.Close()
	}
}

func (o *SlotObserver) Close() error {
	o.once.Do(func() {
		o.cancel()
		o.mu.Lock()
		o.closed = true
		children := make([]*managedSlotObserver, 0, len(o.slots))
		for _, child := range o.slots {
			children = append(children, child)
		}
		clear(o.slots)
		o.mu.Unlock()
		for _, child := range children {
			child.cancelFanIn()
			_ = child.observer.Close()
		}
		o.wg.Wait()
	})
	return nil
}
