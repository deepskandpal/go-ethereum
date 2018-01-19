// Copyright 2018 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package testing

import (
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p/discover"
	"github.com/ethereum/go-ethereum/p2p/simulations"
	"github.com/ethereum/go-ethereum/p2p/simulations/adapters"
	"github.com/ethereum/go-ethereum/swarm/network"
	"github.com/ethereum/go-ethereum/swarm/storage"
)

type Simulation struct {
	Net    *simulations.Network
	Stores []storage.ChunkStore
	Addrs  []network.Addr
	IDs    []discover.NodeID
}

func SetStores(addrs ...network.Addr) ([]storage.ChunkStore, func(), error) {
	var datadirs []string
	stores := make([]storage.ChunkStore, len(addrs))
	var err error
	for i, addr := range addrs {
		var datadir string
		datadir, err = ioutil.TempDir("", "streamer")
		if err != nil {
			break
		}
		var store storage.ChunkStore
		store, err = storage.NewTestLocalStoreForAddr(datadir, addr.Over())
		if err != nil {
			break
		}
		datadirs = append(datadirs, datadir)
		stores[i] = store
	}
	teardown := func() {
		for _, datadir := range datadirs {
			os.RemoveAll(datadir)
		}
	}
	return stores, teardown, err
}

func NewAdapter(adapterType string, services adapters.Services) (adapter adapters.NodeAdapter, teardown func(), err error) {
	teardown = func() {}
	switch adapterType {
	case "sim":
		adapter = adapters.NewSimAdapter(services)
	case "socket":
		adapter = adapters.NewSocketAdapter(services)
	case "exec":
		baseDir, err0 := ioutil.TempDir("", "swarm-test")
		if err0 != nil {
			return nil, teardown, err0
		}
		teardown = func() { os.RemoveAll(baseDir) }
		adapter = adapters.NewExecAdapter(baseDir)
	case "docker":
		adapter, err = adapters.NewDockerAdapter()
		if err != nil {
			return nil, teardown, err
		}
	default:
		return nil, teardown, errors.New("adapter needs to be one of sim, socket, exec, docker")
	}
	return adapter, teardown, nil
}

func CheckResult(t *testing.T, result *simulations.StepResult, startedAt, finishedAt time.Time) {
	t.Logf("Simulation with %d nodes passed in %s", len(result.Passes), result.FinishedAt.Sub(result.StartedAt))
	var min, max time.Duration
	var sum int
	for _, pass := range result.Passes {
		duration := pass.Sub(result.StartedAt)
		if sum == 0 || duration < min {
			min = duration
		}
		if duration > max {
			max = duration
		}
		sum += int(duration.Nanoseconds())
	}
	t.Logf("Min: %s, Max: %s, Average: %s", min, max, time.Duration(sum/len(result.Passes))*time.Nanosecond)
	t.Logf("Setup: %s, Shutdown: %s", result.StartedAt.Sub(startedAt), finishedAt.Sub(result.FinishedAt))
}

type RunConfig struct {
	Adapter   string
	Step      *simulations.Step
	NodeCount int
	ConnLevel int
	ToAddr    func(discover.NodeID) *network.BzzAddr
	Services  adapters.Services
}

func NewSimulation(conf *RunConfig) (*Simulation, func(), error) {
	// create network
	nodes := conf.NodeCount
	adapter, adapterTeardown, err := NewAdapter(conf.Adapter, conf.Services)
	if err != nil {
		return nil, adapterTeardown, err
	}
	net := simulations.NewNetwork(adapter, &simulations.NetworkConfig{
		ID:             "0",
		DefaultService: "streamer",
	})
	teardown := func() {
		adapterTeardown()
		net.Shutdown()
	}
	ids := make([]discover.NodeID, nodes)
	addrs := make([]network.Addr, nodes)
	// start nodes
	for i := 0; i < nodes; i++ {
		node, err := net.NewNode()
		if err != nil {
			return nil, teardown, fmt.Errorf("error creating node: %s", err)
		}
		ids[i] = node.ID()
		addrs[i] = conf.ToAddr(ids[i])
	}
	// set nodes number of Stores available
	stores, storeTeardown, err := SetStores(addrs...)
	teardown = func() {
		storeTeardown()
		adapterTeardown()
		net.Shutdown()
	}
	if err != nil {
		return nil, teardown, err
	}
	s := &Simulation{
		Net:    net,
		Stores: stores,
		IDs:    ids,
		Addrs:  addrs,
	}
	return s, teardown, nil
}

func (s *Simulation) Run(conf *RunConfig) (*simulations.StepResult, error) {
	// bring up nodes, launch the servive
	nodes := conf.NodeCount
	conns := conf.ConnLevel
	for i := 0; i < nodes; i++ {
		if err := s.Net.Start(s.IDs[i]); err != nil {
			return nil, fmt.Errorf("error starting node %s: %s", s.IDs[i].TerminalString(), err)
		}
	}
	// run a simulation which connects the 10 nodes in a chain
	wg := sync.WaitGroup{}
	for i := range s.IDs {
		// collect the overlay addresses, to
		for j := 0; j < conns; j++ {
			var k int
			if j == 0 {
				k = i - 1
			} else {
				k = rand.Intn(len(s.IDs))
			}
			if i > 0 {
				wg.Add(1)
				go func(i, k int) {
					defer wg.Done()
					s.Net.Connect(s.IDs[i], s.IDs[k])
				}(i, k)
			}
		}
	}
	wg.Wait()

	log.Debug(fmt.Sprintf("nodes: %v", len(s.Addrs)))

	// create an only locally retrieving dpa for the pivot node to test
	// if retriee requests have arrived
	timeout := 300 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	result := simulations.NewSimulation(s.Net).Run(ctx, conf.Step)
	return result, nil
}
