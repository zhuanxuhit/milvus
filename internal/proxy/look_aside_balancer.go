// Licensed to the LF AI & Data foundation under one
// or more contributor license agreements. See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership. The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License. You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package proxy

import (
	"context"
	"math"
	"math/rand"
	"strconv"
	"sync"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/milvus-io/milvus-proto/go-api/v2/commonpb"
	"github.com/milvus-io/milvus/internal/proto/internalpb"
	"github.com/milvus-io/milvus/pkg/log"
	"github.com/milvus-io/milvus/pkg/metrics"
	"github.com/milvus-io/milvus/pkg/util/conc"
	"github.com/milvus-io/milvus/pkg/util/merr"
	"github.com/milvus-io/milvus/pkg/util/paramtable"
	"github.com/milvus-io/milvus/pkg/util/typeutil"
	"go.uber.org/atomic"
	"go.uber.org/zap"
)

type LookAsideBalancer struct {
	clientMgr shardClientMgr

	// query node -> workload latest metrics
	metricsMap *typeutil.ConcurrentMap[int64, *internalpb.CostAggregation]

	// query node -> last update metrics ts
	metricsUpdateTs *typeutil.ConcurrentMap[int64, int64]

	// query node -> total nq of requests which already send but response hasn't received
	executingTaskTotalNQ *typeutil.ConcurrentMap[int64, *atomic.Int64]

	unreachableQueryNodes *typeutil.ConcurrentSet[int64]

	// query node id -> number of consecutive heartbeat failures
	failedHeartBeatCounter *typeutil.ConcurrentMap[int64, *atomic.Int64]

	closeCh   chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

func NewLookAsideBalancer(clientMgr shardClientMgr) *LookAsideBalancer {
	balancer := &LookAsideBalancer{
		clientMgr:              clientMgr,
		metricsMap:             typeutil.NewConcurrentMap[int64, *internalpb.CostAggregation](),
		metricsUpdateTs:        typeutil.NewConcurrentMap[int64, int64](),
		executingTaskTotalNQ:   typeutil.NewConcurrentMap[int64, *atomic.Int64](),
		unreachableQueryNodes:  typeutil.NewConcurrentSet[int64](),
		failedHeartBeatCounter: typeutil.NewConcurrentMap[int64, *atomic.Int64](),
		closeCh:                make(chan struct{}),
	}

	return balancer
}

func (b *LookAsideBalancer) Start(ctx context.Context) {
	b.wg.Add(1)
	go b.checkQueryNodeHealthLoop(ctx)
}

func (b *LookAsideBalancer) Close() {
	b.closeOnce.Do(func() {
		close(b.closeCh)
		b.wg.Wait()
	})
}

func (b *LookAsideBalancer) SelectNode(ctx context.Context, availableNodes []int64, cost int64) (int64, error) {
	log := log.Ctx(ctx).WithRateGroup("proxy.LookAsideBalancer", 1, 60)
	targetNode := int64(-1)
	targetScore := float64(math.MaxFloat64)
	rand.Shuffle(len(availableNodes), func(i, j int) {
		availableNodes[i], availableNodes[j] = availableNodes[j], availableNodes[i]
	})
	for _, node := range availableNodes {
		if b.unreachableQueryNodes.Contain(node) {
			log.RatedWarn(5, "query node  is unreachable, skip it",
				zap.Int64("nodeID", node))
			continue
		}

		cost, _ := b.metricsMap.Get(node)
		executingNQ, ok := b.executingTaskTotalNQ.Get(node)
		if !ok {
			executingNQ = atomic.NewInt64(0)
			b.executingTaskTotalNQ.Insert(node, executingNQ)
		}

		score := b.calculateScore(node, cost, executingNQ.Load())
		metrics.ProxyWorkLoadScore.WithLabelValues(strconv.FormatInt(node, 10)).Set(score)

		if targetNode == -1 || score < targetScore {
			targetScore = score
			targetNode = node
		}
	}

	if targetNode == -1 {
		return -1, merr.WrapErrServiceUnavailable("all available nodes are unreachable")
	}

	// update executing task cost
	totalNQ, _ := b.executingTaskTotalNQ.Get(targetNode)
	nq := totalNQ.Add(cost)
	metrics.ProxyExecutingTotalNq.WithLabelValues(strconv.FormatInt(paramtable.GetNodeID(), 10)).Set(float64(nq))

	return targetNode, nil
}

// when task canceled, should reduce executing total nq cost
func (b *LookAsideBalancer) CancelWorkload(node int64, nq int64) {
	totalNQ, ok := b.executingTaskTotalNQ.Get(node)
	if ok {
		nq := totalNQ.Sub(nq)
		metrics.ProxyExecutingTotalNq.WithLabelValues(strconv.FormatInt(paramtable.GetNodeID(), 10)).Set(float64(nq))
	}
}

// UpdateCostMetrics used for cache some metrics of recent search/query cost
func (b *LookAsideBalancer) UpdateCostMetrics(node int64, cost *internalpb.CostAggregation) {
	// cache the latest query node cost metrics for updating the score
	if cost != nil {
		b.metricsMap.Insert(node, cost)
	}
	b.metricsUpdateTs.Insert(node, time.Now().UnixMilli())
}

// calculateScore compute the query node's workload score
// https://www.usenix.org/conference/nsdi15/technical-sessions/presentation/suresh
func (b *LookAsideBalancer) calculateScore(node int64, cost *internalpb.CostAggregation, executingNQ int64) float64 {
	if cost == nil || cost.GetResponseTime() == 0 {
		return math.Pow(float64(executingNQ), 3.0)
	}

	// for multi-replica cases, when there are no task which waiting in queue,
	// the response time will effect the score, to prevent the score based on a too old value
	// we expire the cost metrics by second if no task in queue.
	if executingNQ == 0 && b.isNodeCostMetricsTooOld(node) {
		return 0
	}

	executeSpeed := float64(cost.ResponseTime) - float64(cost.ServiceTime)
	if executingNQ < 0 {
		log.Warn("unexpected executing nq value",
			zap.Int64("executingNQ", executingNQ))
		return executeSpeed
	}

	if cost.GetTotalNQ() < 0 {
		log.Warn("unexpected total nq value",
			zap.Int64("totalNq", cost.GetTotalNQ()))
		return executeSpeed
	}

	workload := math.Pow(float64(1+cost.GetTotalNQ()+executingNQ), 3.0) * float64(cost.ServiceTime)
	if workload < 0 {
		return math.MaxFloat64
	}

	return executeSpeed + workload
}

// if the node cost metrics hasn't been updated for a second, we think the metrics is too old
func (b *LookAsideBalancer) isNodeCostMetricsTooOld(node int64) bool {
	lastUpdateTs, ok := b.metricsUpdateTs.Get(node)
	if !ok || lastUpdateTs == 0 {
		return false
	}

	return time.Now().UnixMilli()-lastUpdateTs > Params.ProxyCfg.CostMetricsExpireTime.GetAsInt64()
}

func (b *LookAsideBalancer) checkQueryNodeHealthLoop(ctx context.Context) {
	log := log.Ctx(ctx).WithRateGroup("proxy.LookAsideBalancer", 1, 60)
	defer b.wg.Done()

	checkQueryNodeHealthInterval := Params.ProxyCfg.CheckQueryNodeHealthInterval.GetAsDuration(time.Millisecond)
	ticker := time.NewTicker(checkQueryNodeHealthInterval)
	defer ticker.Stop()
	log.Info("Start check query node health loop")
	pool := conc.NewDefaultPool[any]()
	for {
		select {
		case <-b.closeCh:
			log.Info("check query node health loop exit")
			return

		case <-ticker.C:
			now := time.Now().UnixMilli()
			var futures []*conc.Future[any]

			// remove old query node from metricsUpdateTs
			b.failedHeartBeatCounter.Range(func(node int64, _ *atomic.Int64) bool {
				_, err := b.clientMgr.GetClient(context.Background(), node)
				if errors.Is(err, errNodeNotExist) {
					b.metricsUpdateTs.GetAndRemove(node)
					b.failedHeartBeatCounter.GetAndRemove(node)
					b.unreachableQueryNodes.Remove(node)
				}
				return true
			})

			b.metricsUpdateTs.Range(func(node int64, lastUpdateTs int64) bool {
				if now-lastUpdateTs > checkQueryNodeHealthInterval.Milliseconds() {
					futures = append(futures, pool.Submit(func() (any, error) {
						checkInterval := Params.ProxyCfg.HealthCheckTimetout.GetAsDuration(time.Millisecond)
						ctx, cancel := context.WithTimeout(context.Background(), checkInterval)
						defer cancel()

						setUnreachable := func(err error) bool {
							failures, ok := b.failedHeartBeatCounter.Get(node)
							if !ok {
								failures = atomic.NewInt64(0)
							}
							failures.Inc()
							b.failedHeartBeatCounter.Insert(node, failures)

							if failures.Load() < Params.ProxyCfg.RetryTimesOnHealthCheck.GetAsInt64() {
								log.Warn("get component status failed",
									zap.Int64("node", node),
									zap.Int64("times", failures.Load()),
									zap.Error(err))
								return false
							}

							return b.unreachableQueryNodes.Insert(node)
						}

						qn, err := b.clientMgr.GetClient(ctx, node)
						if err != nil {
							if setUnreachable(err) {
								log.Warn("get client failed, set node unreachable", zap.Int64("node", node), zap.Error(err))
							}
							return struct{}{}, nil
						}

						resp, err := qn.GetComponentStates(ctx)
						if err != nil {
							if setUnreachable(err) {
								log.Warn("get component status failed,set node unreachable", zap.Int64("node", node), zap.Error(err))
							}
							return struct{}{}, nil
						}

						if resp.GetState().GetStateCode() != commonpb.StateCode_Healthy {
							if setUnreachable(merr.ErrServiceUnavailable) {
								log.Warn("component status unhealthy,set node unreachable", zap.Int64("node", node), zap.Error(err))
							}
							return struct{}{}, nil
						}

						// check health successfully, update check health ts
						b.metricsUpdateTs.Insert(node, time.Now().Local().UnixMilli())
						if b.unreachableQueryNodes.TryRemove(node) {
							// once heartbeat succeed, clear filed counter
							failures, ok := b.failedHeartBeatCounter.Get(node)
							if ok {
								failures.Store(0)
							}
							log.Info("component recuperated, set node reachable", zap.Int64("node", node), zap.Error(err))
						}

						return struct{}{}, nil
					}))
				}

				return true
			})
			conc.AwaitAll(futures...)
		}
	}
}
