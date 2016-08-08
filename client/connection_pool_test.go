// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package client

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/m3db/m3db/generated/thrift/rpc"
	"github.com/m3db/m3db/interfaces/m3db"
	"github.com/m3db/m3db/topology"
	"github.com/m3db/m3x/close"
	"github.com/uber/tchannel-go/thrift"

	"github.com/stretchr/testify/assert"
)

var (
	h           = topology.NewHost("testhost:9000")
	channelNone = &nullChannel{}
)

func newConnectionPoolTestOptions() m3db.ClientOptions {
	return NewOptions().
		BackgroundConnectInterval(5 * time.Millisecond).
		BackgroundConnectStutter(2 * time.Millisecond).
		BackgroundHealthCheckInterval(5 * time.Millisecond).
		BackgroundHealthCheckStutter(2 * time.Millisecond)
}

func TestConnectionPoolConnectsAndRetriesConnects(t *testing.T) {
	// Scenario:
	// 1. Try fill 4 connections
	// > Fail 1 on connection step, have 3 connections
	// 2. Try fill remaining connection
	// > Fail 1 on health check, have 3 connections
	// 3. Try fill remaining connection
	// > Fulfill remaining connection, have 4 connections
	// 4. Don't bother

	var (
		attempts        int32
		sleeps          int32
		rounds          int32
		sleepWgs        [4]sync.WaitGroup
		proceedSleepWgs [3]sync.WaitGroup
		doneWg          sync.WaitGroup
	)
	for i := range sleepWgs {
		sleepWgs[i].Add(1)
	}
	for i := range proceedSleepWgs {
		proceedSleepWgs[i].Add(1)
	}
	doneWg.Add(1)

	opts := newConnectionPoolTestOptions()
	opts = opts.MaxConnectionCount(4)
	conns := newConnectionPool(h, opts).(*connPool)
	conns.newConn = func(ch string, addr string, opts m3db.ClientOptions) (xclose.SimpleCloser, rpc.TChanNode, error) {
		attempt := int(atomic.AddInt32(&attempts, 1))
		if attempt == 1 {
			return nil, nil, fmt.Errorf("a connect error")
		}
		return channelNone, nil, nil
	}
	conns.healthCheckNewConn = func(client rpc.TChanNode, opts m3db.ClientOptions) error {
		if atomic.LoadInt32(&rounds) == 1 {
			// If second round then fail health check
			return fmt.Errorf("a health check error")
		}
		return nil
	}
	conns.healthCheck = func(client rpc.TChanNode, opts m3db.ClientOptions) error {
		return nil
	}
	conns.sleepConnect = func(t time.Duration) {
		sleep := int(atomic.AddInt32(&sleeps, 1))
		if sleep <= 4 {
			if sleep <= len(sleepWgs) {
				sleepWgs[sleep-1].Done()
			}
			if sleep <= len(proceedSleepWgs) {
				proceedSleepWgs[sleep-1].Wait()
			}
		}
		if sleep == 4 {
			doneWg.Wait()
			return // All done
		}
		atomic.AddInt32(&rounds, 1)
		time.Sleep(time.Millisecond)
	}

	assert.Equal(t, 0, conns.GetConnectionCount())

	conns.Open()

	// Wait for first round, should've created all conns except first
	sleepWgs[0].Wait()
	assert.Equal(t, 3, conns.GetConnectionCount())
	proceedSleepWgs[0].Done()

	// Wait for second round, all attempts should succeed but all fail health checks
	sleepWgs[1].Wait()
	assert.Equal(t, 3, conns.GetConnectionCount())
	proceedSleepWgs[1].Done()

	// Wait for third round, now should succeed and all connections accounted for
	sleepWgs[2].Wait()
	assert.Equal(t, 4, conns.GetConnectionCount())
	doneAll := attempts
	proceedSleepWgs[2].Done()

	// Wait for fourth roundm, now should not involve attempting to spawn connections
	sleepWgs[3].Wait()
	// Ensure no more attempts done in fnal round
	assert.Equal(t, doneAll, attempts)

	conns.Close()
	doneWg.Done()

	nextClient, err := conns.NextClient()
	assert.Nil(t, nextClient)
	assert.Equal(t, errConnectionPoolClosed, err)
}

func TestConnectionPoolHealthChecks(t *testing.T) {
	// Scenario:
	// 1. Fill 2 connections
	// 2. Round 1, fail conn 0 health checks
	// > Take connection out
	// 3. Round 2, fail conn 1 health checks
	// > Take connection out

	var (
		newConnAttempt int32
		connectRounds  int32
		healthRounds   int32
		invokeFail     int32
		client1        = &nullNodeClient{}
		client2        = &nullNodeClient{}
		overrides      = []healthCheckFn{}
		overridesMut   sync.RWMutex
		pushOverride   = func(fn healthCheckFn) {
			overridesMut.Lock()
			defer overridesMut.Unlock()
			overrides = append(overrides, fn)
		}
		popOverride = func() healthCheckFn {
			overridesMut.Lock()
			defer overridesMut.Unlock()
			if len(overrides) == 0 {
				return nil
			}
			next := overrides[0]
			overrides = overrides[1:]
			return next
		}
		pushFailClientOverride = func(failTargetClient rpc.TChanNode) {
			var failOverride healthCheckFn
			failOverride = func(client rpc.TChanNode, opts m3db.ClientOptions) error {
				if client == failTargetClient {
					atomic.AddInt32(&invokeFail, 1)
					return fmt.Errorf("fail client")
				}
				// Not failing this client, re-enqueue
				pushOverride(failOverride)
				return nil
			}
			pushOverride(failOverride)
		}
		failsDoneWg [2]sync.WaitGroup
	)
	for i := range failsDoneWg {
		failsDoneWg[i].Add(1)
	}

	opts := newConnectionPoolTestOptions()
	opts = opts.MaxConnectionCount(2)
	conns := newConnectionPool(h, opts).(*connPool)
	conns.newConn = func(ch string, addr string, opts m3db.ClientOptions) (xclose.SimpleCloser, rpc.TChanNode, error) {
		attempt := atomic.AddInt32(&newConnAttempt, 1)
		if attempt == 1 {
			return channelNone, client1, nil
		} else if attempt == 2 {
			return channelNone, client2, nil
		}
		return nil, nil, fmt.Errorf("spawning only 2 connections")
	}
	conns.healthCheckNewConn = func(client rpc.TChanNode, opts m3db.ClientOptions) error {
		return nil
	}
	conns.healthCheck = func(client rpc.TChanNode, opts m3db.ClientOptions) error {
		if fn := popOverride(); fn != nil {
			return fn(client, opts)
		}
		return nil
	}
	conns.sleepConnect = func(t time.Duration) {
		atomic.AddInt32(&connectRounds, 1)
		time.Sleep(time.Millisecond)
	}
	conns.sleepHealth = func(t time.Duration) {
		atomic.AddInt32(&healthRounds, 1)
		if atomic.LoadInt32(&invokeFail) == 1 {
			failsDoneWg[0].Done()
		} else if atomic.LoadInt32(&invokeFail) == 2 {
			failsDoneWg[1].Done()
		}
		time.Sleep(time.Millisecond)
	}

	assert.Equal(t, 0, conns.GetConnectionCount())

	conns.Open()

	// Wait for first round, should've created all conns except first
	for atomic.LoadInt32(&connectRounds) < 1 {
		time.Sleep(time.Millisecond)
	}

	assert.Equal(t, 2, conns.GetConnectionCount())

	// Fail client1 health check
	pushFailClientOverride(client1)

	// Wait for health check round to take action
	failsDoneWg[0].Wait()

	// Verify only 1 connection and its client2
	assert.Equal(t, 1, conns.GetConnectionCount())
	for i := 0; i < 2; i++ {
		nextClient, err := conns.NextClient()
		assert.NoError(t, err)
		assert.Equal(t, client2, nextClient)
	}

	// Fail client2 health check
	pushFailClientOverride(client2)

	// Wait for health check round to take action
	failsDoneWg[1].Wait()
	assert.Equal(t, 0, conns.GetConnectionCount())
	nextClient, err := conns.NextClient()
	assert.Nil(t, nextClient)
	assert.Equal(t, errConnectionPoolHasNoConnections, err)

	conns.Close()

	nextClient, err = conns.NextClient()
	assert.Nil(t, nextClient)
	assert.Equal(t, errConnectionPoolClosed, err)
}

type nullChannel struct{}

func (*nullChannel) Close() {}

type nullNodeClient struct{}

func (*nullNodeClient) Fetch(ctx thrift.Context, req *rpc.FetchRequest) (*rpc.FetchResult_, error) {
	return nil, nil
}

func (*nullNodeClient) FetchRawBatch(ctx thrift.Context, req *rpc.FetchRawBatchRequest) (*rpc.FetchRawBatchResult_, error) {
	return nil, nil
}

func (*nullNodeClient) Health(ctx thrift.Context) (*rpc.HealthResult_, error) {
	return nil, nil
}

func (*nullNodeClient) Write(ctx thrift.Context, req *rpc.WriteRequest) error {
	return nil
}

func (*nullNodeClient) WriteBatch(ctx thrift.Context, req *rpc.WriteBatchRequest) error {
	return nil
}