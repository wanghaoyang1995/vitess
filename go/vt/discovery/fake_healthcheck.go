/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package discovery

import (
	"context"
	"sort"
	"sync"

	"google.golang.org/protobuf/proto"

	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/topo/topoproto"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vttablet/queryservice"
	"vitess.io/vitess/go/vt/vttablet/sandboxconn"

	querypb "vitess.io/vitess/go/vt/proto/query"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
)

// This file contains the definitions for a FakeHealthCheck class to
// simulate a HealthCheck module. Note it is not in a sub-package because
// otherwise it couldn't be used in this package's tests because of
// circular dependencies.

// NewFakeHealthCheck returns the fake healthcheck object.
func NewFakeHealthCheck(ch chan *TabletHealth) *FakeHealthCheck {
	return &FakeHealthCheck{
		items: make(map[string]*fhcItem),
		ch:    ch,
	}
}

// FakeHealthCheck implements discovery.HealthCheck.
type FakeHealthCheck struct {
	// mu protects the items map
	mu               sync.RWMutex
	items            map[string]*fhcItem
	currentTabletUID int
	// channel to return on subscribe. Pass nil if no subscribe should not return a channel
	ch chan *TabletHealth
}

type fhcItem struct {
	ts *TabletHealth
}

//
// discovery.HealthCheck interface methods
//

// RegisterStats is not implemented.
func (fhc *FakeHealthCheck) RegisterStats() {
}

// WaitForAllServingTablets is not implemented.
func (fhc *FakeHealthCheck) WaitForAllServingTablets(ctx context.Context, targets []*querypb.Target) error {
	return nil
}

// GetHealthyTabletStats returns only the healthy tablets - Serving true and LastError is not nil
func (fhc *FakeHealthCheck) GetHealthyTabletStats(target *querypb.Target) []*TabletHealth {
	result := make([]*TabletHealth, 0)
	fhc.mu.Lock()
	defer fhc.mu.Unlock()
	for _, item := range fhc.items {
		if proto.Equal(item.ts.Target, target) && item.ts.Serving && item.ts.LastError == nil {
			result = append(result, item.ts)
		}
	}
	return result
}

// Subscribe returns the channel in the struct. Subscribe should only be called in one place for this fake health check
func (fhc *FakeHealthCheck) Subscribe() chan *TabletHealth {
	return fhc.ch
}

// GetPrimaryTablet gets the primary tablet from the tablets that healthcheck has seen so far
func (fhc *FakeHealthCheck) GetPrimaryTablet() *topodatapb.Tablet {
	fhc.mu.Lock()
	defer fhc.mu.Unlock()
	for _, item := range fhc.items {
		if item.ts.Tablet.GetType() == topodatapb.TabletType_PRIMARY {
			return item.ts.Tablet
		}
	}
	return nil
}

// Broadcast broadcasts the healthcheck for the given tablet
func (fhc *FakeHealthCheck) Broadcast(tablet *topodatapb.Tablet) {
	if fhc.ch == nil {
		return
	}
	fhc.mu.Lock()
	defer fhc.mu.Unlock()
	key := TabletToMapKey(tablet)
	item, isPresent := fhc.items[key]
	if !isPresent {
		return
	}
	fhc.ch <- simpleCopy(item.ts)
}

// SetServing sets the serving variable for the given tablet
func (fhc *FakeHealthCheck) SetServing(tablet *topodatapb.Tablet, serving bool) {
	if fhc.ch == nil {
		return
	}
	fhc.mu.Lock()
	defer fhc.mu.Unlock()
	key := TabletToMapKey(tablet)
	item, isPresent := fhc.items[key]
	if !isPresent {
		return
	}
	item.ts.Serving = serving
}

// SetTabletType sets the tablet type for the given tablet
func (fhc *FakeHealthCheck) SetTabletType(tablet *topodatapb.Tablet, tabletType topodatapb.TabletType) {
	if fhc.ch == nil {
		return
	}
	fhc.mu.Lock()
	defer fhc.mu.Unlock()
	key := TabletToMapKey(tablet)
	item, isPresent := fhc.items[key]
	if !isPresent {
		return
	}
	item.ts.Tablet.Type = tabletType
	tablet.Type = tabletType
	item.ts.Target.TabletType = tabletType
}

// Unsubscribe is not implemented.
func (fhc *FakeHealthCheck) Unsubscribe(c chan *TabletHealth) {
}

// AddTablet adds the tablet.
func (fhc *FakeHealthCheck) AddTablet(tablet *topodatapb.Tablet) {
	key := TabletToMapKey(tablet)
	item := &fhcItem{
		ts: &TabletHealth{
			Tablet: tablet,
			Target: &querypb.Target{
				Keyspace:   tablet.Keyspace,
				Shard:      tablet.Shard,
				TabletType: tablet.Type,
			},
			Serving: true,
			Stats:   &querypb.RealtimeStats{},
		},
	}

	fhc.mu.Lock()
	defer fhc.mu.Unlock()
	fhc.items[key] = item
}

// RemoveTablet removes the tablet.
func (fhc *FakeHealthCheck) RemoveTablet(tablet *topodatapb.Tablet) {
	fhc.mu.Lock()
	defer fhc.mu.Unlock()
	key := TabletToMapKey(tablet)
	item, ok := fhc.items[key]
	if !ok {
		return
	}
	// Make sure the key still corresponds to the tablet we want to delete.
	// If it doesn't match, we should do nothing. The tablet we were asked to
	// delete is already gone, and some other tablet is using the key
	// (host:port) that the original tablet used to use, which is fine.
	if !topoproto.TabletAliasEqual(tablet.Alias, item.ts.Tablet.Alias) {
		return
	}
	delete(fhc.items, key)
}

// ReplaceTablet removes the old tablet and adds the new.
func (fhc *FakeHealthCheck) ReplaceTablet(old, new *topodatapb.Tablet) {
	fhc.RemoveTablet(old)
	fhc.AddTablet(new)
}

// TabletConnection returns the TabletConn of the given tablet.
func (fhc *FakeHealthCheck) TabletConnection(alias *topodatapb.TabletAlias, target *querypb.Target) (queryservice.QueryService, error) {
	aliasStr := topoproto.TabletAliasString(alias)
	fhc.mu.RLock()
	defer fhc.mu.RUnlock()
	for _, item := range fhc.items {
		if proto.Equal(alias, item.ts.Tablet.Alias) {
			return item.ts.Conn, nil
		}
	}
	return nil, vterrors.Errorf(vtrpcpb.Code_NOT_FOUND, "tablet %v not found", aliasStr)
}

// CacheStatus returns the status for each tablet
func (fhc *FakeHealthCheck) CacheStatus() TabletsCacheStatusList {
	fhc.mu.Lock()
	defer fhc.mu.Unlock()

	stats := make(TabletsCacheStatusList, 0, len(fhc.items))
	for _, item := range fhc.items {
		stats = append(stats, &TabletsCacheStatus{
			Cell:         "FakeCell",
			Target:       item.ts.Target,
			TabletsStats: TabletStatsList{item.ts},
		})
	}
	sort.Sort(stats)
	return stats
}

// Close is not implemented.
func (fhc *FakeHealthCheck) Close() error {
	return nil
}

//
// Management methods
//

// Reset cleans up the internal state.
func (fhc *FakeHealthCheck) Reset() {
	fhc.mu.Lock()
	defer fhc.mu.Unlock()

	fhc.items = make(map[string]*fhcItem)
	fhc.currentTabletUID = 0
}

// AddFakeTablet inserts a fake entry into FakeHealthCheck.
// The Tablet can be talked to using the provided connection.
// The Listener is called, as if AddTablet had been called.
// For flexibility the connection is created via a connFactory callback
func (fhc *FakeHealthCheck) AddFakeTablet(cell, host string, port int32, keyspace, shard string, tabletType topodatapb.TabletType, serving bool, reparentTS int64, err error, connFactory func(*topodatapb.Tablet) queryservice.QueryService) queryservice.QueryService {
	fhc.mu.Lock()
	defer fhc.mu.Unlock()

	// tabletUID must be unique
	fhc.currentTabletUID++
	uid := fhc.currentTabletUID
	t := topo.NewTablet(uint32(uid), cell, host)
	t.Keyspace = keyspace
	t.Shard = shard
	t.Type = tabletType
	t.PortMap["vt"] = port
	key := TabletToMapKey(t)

	item := fhc.items[key]
	if item == nil {
		item = &fhcItem{
			ts: &TabletHealth{
				Tablet: t,
			},
		}
		fhc.items[key] = item
	}
	item.ts.Target = &querypb.Target{
		Keyspace:   keyspace,
		Shard:      shard,
		TabletType: tabletType,
	}
	item.ts.Serving = serving
	item.ts.PrimaryTermStartTime = reparentTS
	item.ts.Stats = &querypb.RealtimeStats{}
	item.ts.LastError = err
	conn := connFactory(t)
	item.ts.Conn = conn

	return conn
}

// AddTestTablet adds a fake tablet for tests using the SandboxConn and returns
// the fake connection
func (fhc *FakeHealthCheck) AddTestTablet(cell, host string, port int32, keyspace, shard string, tabletType topodatapb.TabletType, serving bool, reparentTS int64, err error) *sandboxconn.SandboxConn {
	conn := fhc.AddFakeTablet(cell, host, port, keyspace, shard, tabletType, serving, reparentTS, err, func(tablet *topodatapb.Tablet) queryservice.QueryService {
		return sandboxconn.NewSandboxConn(tablet)
	})
	return conn.(*sandboxconn.SandboxConn)
}

// GetAllTablets returns all the tablets we have.
func (fhc *FakeHealthCheck) GetAllTablets() map[string]*topodatapb.Tablet {
	res := make(map[string]*topodatapb.Tablet)
	fhc.mu.RLock()
	defer fhc.mu.RUnlock()
	for key, t := range fhc.items {
		res[key] = t.ts.Tablet
	}
	return res
}

func simpleCopy(th *TabletHealth) *TabletHealth {
	return &TabletHealth{
		Conn:                 th.Conn,
		Tablet:               proto.Clone(th.Tablet).(*topodatapb.Tablet),
		Target:               proto.Clone(th.Target).(*querypb.Target),
		Stats:                proto.Clone(th.Stats).(*querypb.RealtimeStats),
		LastError:            th.LastError,
		PrimaryTermStartTime: th.PrimaryTermStartTime,
		Serving:              th.Serving,
	}
}
