// Copyright (c) 2014 Couchbase, Inc.
// Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file
// except in compliance with the License. You may obtain a copy of the License at
//   http://www.apache.org/licenses/LICENSE-2.0
// Unless required by applicable law or agreed to in writing, software distributed under the
// License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing permissions
// and limitations under the License.
package planner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/couchbase/cbauth"
	"github.com/couchbase/indexing/secondary/common"
	"github.com/couchbase/indexing/secondary/logging"
	"github.com/couchbase/indexing/secondary/manager"
	"net/http"
	"runtime"
	"strings"
	"time"
)

///////////////////////////////////////////////////////
// Function
///////////////////////////////////////////////////////

//
// This function retrieves the index layout plan from a live cluster.
//
func RetrievePlanFromCluster(clusterUrl string) (*Plan, error) {

	indexers, err := getIndexLayout(clusterUrl)
	if err != nil {
		return nil, err
	}

	// If there is no indexer, plan.Placement will be nil.
	plan := &Plan{Placement: indexers,
		MemQuota: 0,
		CpuQuota: 0,
		IsLive:   true,
	}

	err = getIndexStats(clusterUrl, plan)
	if err != nil {
		return nil, err
	}

	err = getIndexSettings(clusterUrl, plan)
	if err != nil {
		return nil, err
	}

	// Recalculate the index and indexer memory and cpu usage using the sizing formaula.
	// The stats retrieved from indexer typically has lower memory/cpu utilization than
	// sizing formula, since sizing forumula captures max usage capacity. By recalculating
	// the usage, it makes sure that planning does not partially skewed data.
	recalculateIndexerSize(plan)

	return plan, nil
}

//
// This function recalculates the index and indexer sizes baesd on sizing formula.
//
func recalculateIndexerSize(plan *Plan) {

	sizing := newMOISizingMethod()

	for _, indexer := range plan.Placement {
		for _, index := range indexer.Indexes {
			sizing.ComputeIndexSize(index)
		}
	}

	for _, indexer := range plan.Placement {
		sizing.ComputeIndexerSize(indexer)
	}
}

//
// This function retrieves the index layout.
//
func getIndexLayout(clusterUrl string) ([]*IndexerNode, error) {

	cinfo, err := clusterInfoCache(clusterUrl)
	if err != nil {
		logging.Errorf("Planner::getIndexLayout: Error from connecting to cluster at %v. Error = %v", clusterUrl, err)
		return nil, err
	}

	// find all nodes that has a index http service
	// If there is any indexer node that is not in active state (e.g. failover), then planner will skip those indexers.
	// Note that if the planner is invoked by the rebalancer, the rebalancer will receive callback ns_server if there is
	// an indexer node fails over while planning is happening.
	nids := cinfo.GetNodesByServiceType(common.INDEX_HTTP_SERVICE)

	list := make([]*IndexerNode, 0)

	for _, nid := range nids {

		// create an empty indexer object using the indexer host name
		node, err := createIndexerNode(cinfo, nid)
		if err != nil {
			logging.Errorf("Planner::getIndexLayout: Error from initializing indexer node. Error = %v", err)
			return nil, err
		}

		// obtain the admin port for the indexer node
		addr, err := cinfo.GetServiceAddress(nid, common.INDEX_HTTP_SERVICE)
		if err != nil {
			logging.Errorf("Planner::getIndexLayout: Error from getting service address for node %v. Error = %v", node.NodeId, err)
			return nil, err
		}

		// Read the index metadata from the indexer node.
		localMeta, err := getLocalMetadata(addr)
		if err != nil {
			logging.Errorf("Planner::getIndexLayout: Error from reading index metadata for node %v. Error = %v", node.NodeId, err)
			return nil, err
		}

		// get the node UUID
		node.NodeUUID = localMeta.NodeUUID

		// Iterate through all the index definition.    For each index definition, create an index usage object.
		for i := 0; i < len(localMeta.IndexDefinitions); i++ {

			defn := &localMeta.IndexDefinitions[i]

			// find the topology metadata
			topology := findTopologyByBucket(localMeta.IndexTopologies, defn.Bucket)
			if topology == nil {
				logging.Errorf("Planner::getIndexLayout: Fail to find index topology for bucket %v for node %v.", defn.Bucket, node.NodeId)
				return nil, err
			}

			// find the index instance from topology metadata
			inst := topology.GetIndexInstByDefn(defn.DefnId)
			if inst == nil {
				logging.Errorf("Planner::getIndexLayout: Fail to find index instance for definition %v for node %v.", defn.DefnId, node.NodeId)
				return nil, err
			}

			// Check the index state.  Only handle index that is active or being built.
			// For index that is in the process of being deleted, planner expects the resource
			// will eventually be freed, so it won't included in planning.
			state, _ := topology.GetStatusByDefn(defn.DefnId)
			if state != common.INDEX_STATE_CREATED &&
				state != common.INDEX_STATE_DELETED &&
				state != common.INDEX_STATE_NIL {

				// create an index usage object
				index := newIndexUsage(defn.DefnId, common.IndexInstId(inst.InstId), defn.Name, defn.Bucket)

				// index is pinned to a node
				if len(defn.Nodes) != 0 {
					index.Hosts = defn.Nodes
				}

				// update sizing
				index.IsPrimary = defn.IsPrimary
				index.IsMOI = (defn.Using == common.IndexType(common.MemoryOptimized) || defn.Using == common.IndexType(common.MemDB))

				// update internal info
				index.Definition = defn
				index.initialNode = node

				node.Indexes = append(node.Indexes, index)
			}
		}

		list = append(list, node)
	}

	return list, nil
}

//
// This function retrieves the index stats.
//
func getIndexStats(clusterUrl string, plan *Plan) error {

	cinfo, err := clusterInfoCache(clusterUrl)
	if err != nil {
		logging.Errorf("Planner::getIndexStats: Error from connecting to cluster at %v. Error = %v", clusterUrl, err)
		return err
	}

	// find all nodes that has a index http service
	nids := cinfo.GetNodesByServiceType(common.INDEX_HTTP_SERVICE)

	for _, nid := range nids {

		// Find the indexer host name
		nodeId, err := getIndexerHost(cinfo, nid)
		if err != nil {
			logging.Errorf("Planner::getIndexStats: Error from initializing indexer node. Error = %v", err)
			return err
		}

		// obtain the admin port for the indexer node
		addr, err := cinfo.GetServiceAddress(nid, common.INDEX_HTTP_SERVICE)
		if err != nil {
			logging.Errorf("Planner::getIndexStats: Error from getting service address for node %v. Error = %v", nodeId, err)
			return err
		}

		// Read the index stats from the indexer node.
		stats, err := getLocalStats(addr)
		if err != nil {
			logging.Errorf("Planner::getIndexStats: Error from reading index stats for node %v. Error = %v", nodeId, err)
			return err
		}

		// look up the corresponding indexer object based on the nodeId
		indexer := findIndexerByNodeId(plan.Placement, nodeId)
		statsMap := stats.ToMap()

		/*
			ServerGroup string `json:"serverGroup,omitempty"`
			CpuUsage    uint64 `json:"cpuUsage,omitempty"`
			DiskUsage   uint64 `json:"diskUsage,omitempty"`
		*/

		var actualStorageMem uint64
		// memory_used_storage constains the total storage consumption,
		// including fdb overhead, main index and back index.  This also
		// includes overhead (skip list / back index).
		if memUsedStorage, ok := statsMap["memory_used_storage"]; ok {
			actualStorageMem = uint64(memUsedStorage.(float64))
		}

		// memory_used is the memory used by indexer.  This includes
		// golang in-use heap space, golang idle heap space, and
		// storage memory manager space (e.g. jemalloc heap space).
		var actualTotalMem uint64
		if memUsed, ok := statsMap["memory_used"]; ok {
			actualTotalMem = uint64(memUsed.(float64))
		}

		// memory_quota is user specified memory quota.
		if memQuota, ok := statsMap["memory_quota"]; ok {
			plan.MemQuota = uint64(memQuota.(float64))
		}

		// uptime
		var elapsed uint64
		if uptimeStat, ok := statsMap["uptime"]; ok {
			uptime := uptimeStat.(string)
			if duration, err := time.ParseDuration(uptime); err == nil {
				elapsed = uint64(duration.Seconds())
			}
		}

		var totalDataSize uint64
		for _, index := range indexer.Indexes {

			/*
				ServerGroup string `json:"serverGroup,omitempty"`
				CpuUsage    uint64 `json:"cpuUsage,omitempty"`
				DiskUsage   uint64 `json:"diskUsage,omitempty"`
			*/

			var key string

			// items_count captures number of key per index
			key = fmt.Sprintf("%v:%v:items_count", index.Bucket, index.Name)
			if itemsCount, ok := statsMap[key]; ok {
				index.NumOfDocs = uint64(itemsCount.(float64))
			}

			// data_size is the total key size of index, excluding back index overhead.
			// Therefore data_size is typically smaller than index sizing equation which
			// includes overhead for back-index.
			key = fmt.Sprintf("%v:%v:data_size", index.Bucket, index.Name)
			if dataSize, ok := statsMap[key]; ok {
				index.ActualMemUsage = uint64(dataSize.(float64))
				totalDataSize += index.ActualMemUsage
			}

			// avg_sec_key_size is currently unavailable in 4.5.   To estimate,
			// the key size, it divides index data_size by items_count.  This
			// contains sec key size + doc key size + main index overhead (74 bytes).
			// Subtract 74 bytes to get sec key size.
			key = fmt.Sprintf("%v:%v:avg_sec_key_size", index.Bucket, index.Name)
			if avgSecKeySize, ok := statsMap[key]; ok {
				index.AvgSecKeySize = uint64(avgSecKeySize.(float64))
			} else if !index.IsPrimary {
				// Aproximate AvgSecKeySize.   AvgSecKeySize includes both
				// sec key len + doc key len
				if index.NumOfDocs != 0 && index.ActualMemUsage != 0 {
					index.ActualKeySize = index.ActualMemUsage / index.NumOfDocs
				}
			}

			// These stats are currently unavailable in 4.5.
			key = fmt.Sprintf("%v:%v:avg_doc_key_size", index.Bucket, index.Name)
			if avgDocKeySize, ok := statsMap[key]; ok {
				index.AvgDocKeySize = uint64(avgDocKeySize.(float64))
			} else if index.IsPrimary {
				// Aproximate AvgDocKeySize.  Subtract 74 bytes for main
				// index overhead
				if index.NumOfDocs != 0 && index.ActualMemUsage != 0 {
					index.ActualKeySize = index.ActualMemUsage / index.NumOfDocs
				}
			}

			// These stats are currently unavailable in 4.5.
			key = fmt.Sprintf("%v:%v:avg_arr_size", index.Bucket, index.Name)
			if avgArrSize, ok := statsMap[key]; ok {
				index.AvgArrSize = uint64(avgArrSize.(float64))
			}

			// These stats are currently unavailable in 4.5.
			key = fmt.Sprintf("%v:%v:avg_arr_key_size", index.Bucket, index.Name)
			if avgArrKeySize, ok := statsMap[key]; ok {
				index.AvgArrKeySize = uint64(avgArrKeySize.(float64))
			}

			// These stats are currently unavailable in 4.5.
			key = fmt.Sprintf("%v:%v:avg_mutation_rate", index.Bucket, index.Name)
			if avgMutationRate, ok := statsMap[key]; ok {
				index.MutationRate = uint64(avgMutationRate.(float64))
			} else {
				key = fmt.Sprintf("%v:%v:num_flush_queued", index.Bucket, index.Name)
				if flushQueuedStat, ok := statsMap[key]; ok {
					flushQueued := uint64(flushQueuedStat.(float64))

					if flushQueued != 0 && elapsed != 0 {
						index.MutationRate = flushQueued / elapsed
					}
				}
			}

			// These stats are currently unavailable in 4.5.
			key = fmt.Sprintf("%v:%v:avg_scan_rate", index.Bucket, index.Name)
			if avgScanRate, ok := statsMap[key]; ok {
				index.ScanRate = uint64(avgScanRate.(float64))
			} else {
				key = fmt.Sprintf("%v:%v:num_rows_returned", index.Bucket, index.Name)
				if rowReturnedStat, ok := statsMap[key]; ok {
					rowReturned := uint64(rowReturnedStat.(float64))

					if rowReturned != 0 && elapsed != 0 {
						index.ScanRate = rowReturned / elapsed
					}
				}
			}
		}

		// compute the estimated memory overhead for each index
		for _, index := range indexer.Indexes {
			ratio := float64(index.ActualMemUsage) / float64(totalDataSize)

			index.ActualMemUsage = uint64(float64(actualStorageMem) * ratio)
			index.ActualMemOverhead = uint64(float64(actualTotalMem-actualStorageMem) * ratio)

			indexer.ActualMemUsage += index.ActualMemUsage
			indexer.ActualMemOverhead += index.ActualMemOverhead
		}
	}

	return nil
}

//
// This function retrieves the index settings.
//
func getIndexSettings(clusterUrl string, plan *Plan) error {

	cinfo, err := clusterInfoCache(clusterUrl)
	if err != nil {
		logging.Errorf("Planner::getIndexSettings: Error from connecting to cluster at %v. Error = %v", clusterUrl, err)
		return err
	}

	// find all nodes that has a index http service
	nids := cinfo.GetNodesByServiceType(common.INDEX_HTTP_SERVICE)

	if len(nids) == 0 {
		logging.Infof("Planner::getIndexSettings: No indexing service.")
		return nil
	}

	// Find the indexer host name
	nodeId, err := getIndexerHost(cinfo, nids[0])
	if err != nil {
		logging.Errorf("Planner::getIndexSettings: Error from initializing indexer node. Error = %v", err)
		return err
	}

	// obtain the admin port for the indexer node
	addr, err := cinfo.GetServiceAddress(nids[0], common.INDEX_HTTP_SERVICE)
	if err != nil {
		logging.Errorf("Planner::getIndexSettings: Error from getting service address for node %v. Error = %v", nodeId, err)
		return err
	}

	// Read the index settings from the indexer node.
	settings, err := getLocalSettings(addr)
	if err != nil {
		logging.Errorf("Planner::getIndexSettings: Error from reading index settings for node %v. Error = %v", nodeId, err)
		return err
	}

	// Find the cpu quota from setting.  If it is set to 0, then find out avail core on the node.
	quota, ok := settings["indexer.settings.max_cpu_percent"]
	if !ok || uint64(quota.(float64)) == 0 {
		plan.CpuQuota = uint64(runtime.NumCPU())
	} else {
		plan.CpuQuota = uint64(quota.(float64) / 100)
	}

	return nil
}

//
// This function extract the topology metadata for a bucket.
//
func findTopologyByBucket(topologies []manager.IndexTopology, bucket string) *manager.IndexTopology {

	for _, topology := range topologies {
		if topology.Bucket == bucket {
			return &topology
		}
	}

	return nil
}

//
// This function finds the index instance id from bucket topology.
//
func findIndexInstId(topology *manager.IndexTopology, defnId common.IndexDefnId) (common.IndexInstId, error) {

	for _, defnRef := range topology.Definitions {
		if defnRef.DefnId == uint64(defnId) {
			for _, inst := range defnRef.Instances {
				return common.IndexInstId(inst.InstId), nil
			}
		}
	}

	return common.IndexInstId(0), errors.New(fmt.Sprintf("Cannot find index instance id for defnition %v", defnId))
}

//
// This function creates an indexer node for plan
//
func createIndexerNode(cinfo *common.ClusterInfoCache, nid common.NodeId) (*IndexerNode, error) {

	host, err := getIndexerHost(cinfo, nid)
	if err != nil {
		return nil, err
	}

	sizing := newMOISizingMethod()
	return newIndexerNode(host, sizing), nil
}

//
// This function gets the indexer host name from ClusterInfoCache.
//
func getIndexerHost(cinfo *common.ClusterInfoCache, nid common.NodeId) (string, error) {

	addr, err := cinfo.GetServiceAddress(nid, "mgmt")
	if err != nil {
		return "", err
	}

	/*
		host, _, err := net.SplitHostPort(addr)
		if err != nil {
			return "", err
		}
		return host, nil
	*/

	return addr, nil
}

//
// This function gets the metadata for a specific indexer host.
//
func getLocalMetadata(addr string) (*manager.LocalIndexMetadata, error) {

	resp, err := getWithCbauth(addr + "/getLocalIndexMetadata")
	if err != nil {
		return nil, err
	}

	localMeta := new(manager.LocalIndexMetadata)
	if err := convertResponse(resp, localMeta); err != nil {
		return nil, err
	}

	return localMeta, nil
}

//
// This function gets the indexer stats for a specific indexer host.
//
func getLocalStats(addr string) (*common.Statistics, error) {

	resp, err := getWithCbauth(addr + "/stats?async=false")
	if err != nil {
		return nil, err
	}

	stats := new(common.Statistics)
	if err := convertResponse(resp, stats); err != nil {
		return nil, err
	}

	return stats, nil
}

//
// This function gets the indexer settings for a specific indexer host.
//
func getLocalSettings(addr string) (map[string]interface{}, error) {

	resp, err := getWithCbauth(addr + "/settings")
	if err != nil {
		return nil, err
	}

	settings := make(map[string]interface{})
	if err := convertResponse(resp, &settings); err != nil {
		return nil, err
	}

	return settings, nil
}

func getWithCbauth(url string) (*http.Response, error) {

	if !strings.HasPrefix(url, "http://") {
		url = "http://" + url
	}

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	cbauth.SetRequestAuthVia(req, nil)

	client := http.Client{Timeout: time.Duration(10 * time.Second)}
	return client.Do(req)
}

//
// This function gets a pointer to clusterInfoCache.
//
func clusterInfoCache(clusterUrl string) (*common.ClusterInfoCache, error) {

	url, err := common.ClusterAuthUrl(clusterUrl)
	if err != nil {
		return nil, err
	}

	cinfo, err := common.NewClusterInfoCache(url, "default")
	if err != nil {
		return nil, err
	}

	if err := cinfo.Fetch(); err != nil {
		return nil, err
	}

	return cinfo, nil
}

//
// This function unmarshalls a response.
//
func convertResponse(r *http.Response, resp interface{}) error {
	defer r.Body.Close()

	buf := new(bytes.Buffer)
	if _, err := buf.ReadFrom(r.Body); err != nil {
		return err
	}

	if err := json.Unmarshal(buf.Bytes(), resp); err != nil {
		return err
	}

	return nil
}

//
// This function find a matching indexer host given the nodeId.
//
func findIndexerByNodeId(indexers []*IndexerNode, nodeId string) *IndexerNode {

	for _, node := range indexers {
		if node.NodeId == nodeId {
			return node
		}
	}

	return nil
}
