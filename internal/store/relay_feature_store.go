package store

import (
	"encoding/json"
	"time"

	"gopkg.in/launchdarkly/go-server-sdk.v4/ldlog"
	"gopkg.in/launchdarkly/ld-relay.v5/logging"

	es "github.com/launchdarkly/eventsource"
	ld "gopkg.in/launchdarkly/go-server-sdk.v4"
)

// ESPublisher defines an interface for publishing events to eventsource
type ESPublisher interface {
	Publish(channels []string, event es.Event)
	PublishComment(channels []string, text string)
	Register(channel string, repo es.Repository)
}

// SSERelayFeatureStore is a feature store that relays updates to eventsource
type SSERelayFeatureStore struct {
	store          ld.FeatureStore
	allPublisher   ESPublisher
	flagsPublisher ESPublisher
	pingPublisher  ESPublisher
	apiKey         string
	loggers        ldlog.Loggers
}

type allRepository struct {
	relayStore *SSERelayFeatureStore
}
type flagsRepository struct {
	relayStore *SSERelayFeatureStore
}
type pingRepository struct {
	relayStore *SSERelayFeatureStore
}

// NewSSERelayFeatureStore creates a new feature store that relays different kinds of updates
func NewSSERelayFeatureStore(apiKey string, allPublisher ESPublisher, flagsPublisher ESPublisher, pingPublisher ESPublisher,
	baseFeatureStore ld.FeatureStore, loggers ldlog.Loggers, heartbeatInterval int) *SSERelayFeatureStore {
	relayStore := &SSERelayFeatureStore{
		store:          baseFeatureStore,
		apiKey:         apiKey,
		allPublisher:   allPublisher,
		flagsPublisher: flagsPublisher,
		pingPublisher:  pingPublisher,
		loggers:        loggers,
	}

	allPublisher.Register(apiKey, allRepository{relayStore: relayStore})
	flagsPublisher.Register(apiKey, flagsRepository{relayStore: relayStore})
	pingPublisher.Register(apiKey, pingRepository{relayStore: relayStore})

	if heartbeatInterval > 0 {
		go func() {
			t := time.NewTicker(time.Duration(heartbeatInterval) * time.Second)
			for {
				relayStore.heartbeat()
				<-t.C
			}
		}()
	}

	return relayStore
}

func (relay *SSERelayFeatureStore) keys() []string {
	return []string{relay.apiKey}
}

func (relay *SSERelayFeatureStore) heartbeat() {
	relay.allPublisher.PublishComment(relay.keys(), "")
	relay.flagsPublisher.PublishComment(relay.keys(), "")
	relay.pingPublisher.PublishComment(relay.keys(), "")
}

// Get returns a single item from the feature store
func (relay *SSERelayFeatureStore) Get(kind ld.VersionedDataKind, key string) (ld.VersionedData, error) {
	return relay.store.Get(kind, key)
}

// All returns all items in the feature store
func (relay *SSERelayFeatureStore) All(kind ld.VersionedDataKind) (map[string]ld.VersionedData, error) {
	return relay.store.All(kind)
}

// Init initializes the feature store
func (relay *SSERelayFeatureStore) Init(allData map[ld.VersionedDataKind]map[string]ld.VersionedData) error {
	relay.loggers.Debug("Received all feature flags")
	err := relay.store.Init(allData)

	if err != nil {
		return err
	}

	relay.allPublisher.Publish(relay.keys(), makePutEvent(allData[ld.Features], allData[ld.Segments]))
	relay.flagsPublisher.Publish(relay.keys(), makeFlagsPutEvent(allData[ld.Features]))
	relay.pingPublisher.Publish(relay.keys(), makePingEvent())

	return nil
}

// Delete marks a single item as deleted in the feature store
func (relay *SSERelayFeatureStore) Delete(kind ld.VersionedDataKind, key string, version int) error {
	relay.loggers.Debugf(`Received feature flag deletion: %s (version %d)`, key, version)
	err := relay.store.Delete(kind, key, version)
	if err != nil {
		return err
	}

	relay.loggers.Debugf(`Feature flag %s was deleted (version %d)`, key, version)
	relay.allPublisher.Publish(relay.keys(), makeDeleteEvent(kind, key, version))
	if kind == ld.Features {
		relay.flagsPublisher.Publish(relay.keys(), makeFlagsDeleteEvent(key, version))
	}
	relay.pingPublisher.Publish(relay.keys(), makePingEvent())

	return nil
}

// Upsert inserts or updates a single item in the feature store
func (relay *SSERelayFeatureStore) Upsert(kind ld.VersionedDataKind, item ld.VersionedData) error {
	relay.loggers.Debugf(`Received feature flag update: %s (version %d)`, item.GetKey(), item.GetVersion())
	err := relay.store.Upsert(kind, item)

	if err != nil {
		return err
	}

	newItem, err := relay.store.Get(kind, item.GetKey())

	if err != nil {
		return err
	}

	if newItem != nil {
		relay.loggers.Debugf(`allPublisher publish event with: %s (version %d)`, newItem.GetKey(), newItem.GetVersion())
		relay.allPublisher.Publish(relay.keys(), makeUpsertEvent(kind, newItem))
		if kind == ld.Features {
			relay.flagsPublisher.Publish(relay.keys(), makeFlagsUpsertEvent(newItem))
		}
		relay.pingPublisher.Publish(relay.keys(), makePingEvent())
	}

	return nil
}

// Initialized returns true after the feature store has been initialized the first time
func (relay *SSERelayFeatureStore) Initialized() bool {
	return relay.store.Initialized()
}

// Replay allows the feature store to act as an SSE repository (to send bootstrap events)
func (r flagsRepository) Replay(channel, id string) (out chan es.Event) {
	out = make(chan es.Event)
	go func() {
		defer close(out)
		if r.relayStore.Initialized() {
			flags, err := r.relayStore.All(ld.Features)

			if err != nil {
				logging.GlobalLoggers.Errorf("Error getting all flags: %s\n", err.Error())
			} else {
				out <- makeFlagsPutEvent(flags)
			}
		}
	}()
	return
}

// Replay allows the feature store to act as an SSE repository (to send bootstrap events)
func (r allRepository) Replay(channel, id string) (out chan es.Event) {
	out = make(chan es.Event)
	go func() {
		defer close(out)
		if r.relayStore.Initialized() {
			flags, err := r.relayStore.All(ld.Features)

			if err != nil {
				logging.GlobalLoggers.Errorf("Error getting all flags: %s\n", err.Error())
			} else {
				segments, err := r.relayStore.All(ld.Segments)
				if err != nil {
					logging.GlobalLoggers.Errorf("Error getting all segments: %s\n", err.Error())
				} else {
					out <- makePutEvent(flags, segments)
				}
			}

		}
	}()
	return
}

// Replay allows the feature store to act as an SSE repository (to send bootstrap events)
func (r pingRepository) Replay(channel, id string) (out chan es.Event) {
	out = make(chan es.Event)
	go func() {
		defer close(out)
		out <- makePingEvent()
	}()
	return
}

var dataKindApiName = map[ld.VersionedDataKind]string{
	ld.Features: "flags",
	ld.Segments: "segments",
}

type flagsPutEvent map[string]ld.VersionedData
type allPutEvent struct {
	D map[string]map[string]ld.VersionedData `json:"data"`
}
type deleteEvent struct {
	Path    string `json:"path"`
	Version int    `json:"version"`
}

type upsertEvent struct {
	Path string           `json:"path"`
	D    ld.VersionedData `json:"data"`
}

type pingEvent struct{}

func (t flagsPutEvent) Id() string {
	return ""
}

func (t flagsPutEvent) Event() string {
	return "put"
}

func (t flagsPutEvent) Data() string {
	data, _ := json.Marshal(t)

	return string(data)
}

func (t flagsPutEvent) Comment() string {
	return ""
}

func (t allPutEvent) Id() string {
	return ""
}

func (t allPutEvent) Event() string {
	return "put"
}

func (t allPutEvent) Data() string {
	data, _ := json.Marshal(t)

	return string(data)
}

func (t allPutEvent) Comment() string {
	return ""
}

func (t upsertEvent) Id() string {
	return ""
}

func (t upsertEvent) Event() string {
	return "patch"
}

func (t upsertEvent) Data() string {
	data, _ := json.Marshal(t)

	return string(data)
}

func (t upsertEvent) Comment() string {
	return ""
}

func (t deleteEvent) Id() string {
	return ""
}

func (t deleteEvent) Event() string {
	return "delete"
}

func (t deleteEvent) Data() string {
	data, _ := json.Marshal(t)

	return string(data)
}

func (t deleteEvent) Comment() string {
	return ""
}

func (t pingEvent) Id() string {
	return ""
}

func (t pingEvent) Event() string {
	return "ping"
}

func (t pingEvent) Data() string {
	return " " // We need something or the data field is not published by eventsource causing the event to be ignored
}

func (t pingEvent) Comment() string {
	return ""
}

func makeUpsertEvent(kind ld.VersionedDataKind, item ld.VersionedData) es.Event {
	return upsertEvent{
		Path: "/" + dataKindApiName[kind] + "/" + item.GetKey(),
		D:    item,
	}
}

func makeFlagsUpsertEvent(item ld.VersionedData) es.Event {
	return upsertEvent{
		Path: "/" + item.GetKey(),
		D:    item,
	}
}

func makeDeleteEvent(kind ld.VersionedDataKind, key string, version int) es.Event {
	return deleteEvent{
		Path:    "/" + dataKindApiName[kind] + "/" + key,
		Version: version,
	}
}

func makeFlagsDeleteEvent(key string, version int) es.Event {
	return deleteEvent{
		Path:    "/" + key,
		Version: version,
	}
}

func makePutEvent(flags map[string]ld.VersionedData, segments map[string]ld.VersionedData) es.Event {
	var allData = map[string]map[string]ld.VersionedData{
		"flags":    {},
		"segments": {},
	}
	for key, flag := range flags {
		allData["flags"][key] = flag
	}
	for key, seg := range segments {
		allData["segments"][key] = seg
	}
	return allPutEvent{D: allData}
}

func makeFlagsPutEvent(flags map[string]ld.VersionedData) es.Event {
	return flagsPutEvent(flags)
}

func makePingEvent() es.Event {
	return pingEvent{}
}
