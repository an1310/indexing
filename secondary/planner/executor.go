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
	"encoding/json"
	"errors"
	"fmt"
	"github.com/couchbase/cbauth/service"
	"github.com/couchbase/indexing/secondary/common"
	"github.com/couchbase/indexing/secondary/logging"
	"io/ioutil"
	"math"
	"math/rand"
	"os"
	"strconv"
	"time"
)

//////////////////////////////////////////////////////////////
// Concrete Type/Struct
/////////////////////////////////////////////////////////////

type RunConfig struct {
	Detail         bool
	GenStmt        string
	MemQuotaFactor float64
	CpuQuotaFactor float64
	Resize         bool
	MaxNumNode     int
	Output         string
	Shuffle        int
	AllowMove      bool
	AllowSwap      bool
	AllowUnpin     bool
	AddNode        int
	DeleteNode     int
	MaxMemUse      int
	MaxCpuUse      int
	MemQuota       int64
	CpuQuota       int
	DataCostWeight float64
	CpuCostWeight  float64
	MemCostWeight  float64
	EjectOnly      bool
}

type RunStats struct {
	AvgIndexSize    float64
	StdDevIndexSize float64
	AvgIndexCpu     float64
	StdDevIndexCpu  float64
	MemoryQuota     uint64
	CpuQuota        uint64
	IndexCount      uint64

	Initial_score             float64
	Initial_indexCount        uint64
	Initial_indexerCount      uint64
	Initial_avgIndexSize      float64
	Initial_stdDevIndexSize   float64
	Initial_avgIndexCpu       float64
	Initial_stdDevIndexCpu    float64
	Initial_avgIndexerSize    float64
	Initial_stdDevIndexerSize float64
	Initial_avgIndexerCpu     float64
	Initial_stdDevIndexerCpu  float64
	Initial_movedIndex        uint64
	Initial_movedData         uint64
}

type Plan struct {
	// placement of indexes	in nodes
	Placement []*IndexerNode `json:"placement,omitempty"`
	MemQuota  uint64         `json:"memQuota,omitempty"`
	CpuQuota  uint64         `json:"cpuQuota,omitempty"`
	IsLive    bool           `json:"isLive,omitempty"`
}

type IndexSpec struct {
	// definition
	Name         string   `json:"name,omitempty"`
	Bucket       string   `json:"bucket,omitempty"`
	IsPrimary    bool     `json:"isPrimary,omitempty"`
	SecExprs     []string `json:"secExprs,omitempty"`
	WhereExpr    string   `json:"where,omitempty"`
	Deferred     bool     `json:"deferred,omitempty"`
	Immutable    bool     `json:"immutable,omitempty"`
	IsArrayIndex bool     `json:"isArrayIndex,omitempty"`

	// usage
	Replica      uint64 `json:"replica,omitempty"`
	NumDoc       uint64 `json:"numDoc,omitempty"`
	DocKeySize   uint64 `json:"docKeySize,omitempty"`
	SecKeySize   uint64 `json:"secKeySize,omitempty"`
	ArrKeySize   uint64 `json:"arrKeySize,omitempty"`
	ArrSize      uint64 `json:"arrSize,omitempty"`
	MutationRate uint64 `json:"mutationRate,omitempty"`
	ScanRate     uint64 `json:"scanRate,omitempty"`
}

//////////////////////////////////////////////////////////////
// Integration with Rebalancer
/////////////////////////////////////////////////////////////

type TransferToken struct {
	MasterId  string
	SourceId  string
	DestId    string
	RebalId   string
	State     string
	InstId    common.IndexInstId
	IndexDefn common.IndexDefn
}

func ExecuteRebalance(clusterUrl string, topologyChange service.TopologyChange, masterId string, ejectOnly bool) (map[string]*TransferToken, error) {
	return ExecuteRebalanceInternal(clusterUrl, topologyChange, masterId, false, false, ejectOnly)
}

func ExecuteRebalanceInternal(clusterUrl string,
	topologyChange service.TopologyChange, masterId string, addNode bool, detail bool, ejectOnly bool) (map[string]*TransferToken, error) {

	plan, err := RetrievePlanFromCluster(clusterUrl)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("Unable to read index layout from cluster %v. err = %s", clusterUrl, err))
	}

	nodes := make(map[string]string)
	for _, node := range plan.Placement {
		nodes[node.NodeUUID] = node.NodeId
	}

	deleteNodes := make([]string, len(topologyChange.EjectNodes))
	for i, node := range topologyChange.EjectNodes {
		if _, ok := nodes[string(node.NodeID)]; !ok {
			return nil, errors.New(fmt.Sprintf("Unable to find indexer node with node UUID %v", node.NodeID))
		}
		deleteNodes[i] = nodes[string(node.NodeID)]
	}

	var numNode int
	if addNode {
		numNode = len(deleteNodes)
	}

	config := DefaultRunConfig()
	config.Detail = detail
	config.Resize = false
	config.AddNode = numNode
	config.EjectOnly = ejectOnly

	p, _, err := execute(config, CommandRebalance, plan, nil, deleteNodes)
	if err != nil {
		return nil, err
	}

	if detail {
		logging.Infof("************ Indexer Layout *************")
		p.PrintLayout()
		logging.Infof("****************************************")
	}

	return genTransferToken(p.Result, masterId, topologyChange), nil
}

func genTransferToken(solution *Solution, masterId string, topologyChange service.TopologyChange) map[string]*TransferToken {

	tokens := make(map[string]*TransferToken)

	for _, indexer := range solution.Placement {
		for _, index := range indexer.Indexes {
			if index.initialNode.NodeId != indexer.NodeId {
				token := &TransferToken{
					MasterId:  masterId,
					SourceId:  index.initialNode.NodeUUID,
					DestId:    indexer.NodeUUID,
					RebalId:   topologyChange.ID,
					State:     "TransferTokenCreated",
					InstId:    index.InstId,
					IndexDefn: *index.Definition,
				}

				ustr, _ := common.NewUUID()
				ttid := fmt.Sprintf("TransferToken%s", ustr.Str())

				tokens[ttid] = token
			}
		}
	}

	return tokens
}

//////////////////////////////////////////////////////////////
// Execution
/////////////////////////////////////////////////////////////

func ExecutePlanWithOptions(plan *Plan, indexSpecs []*IndexSpec, detail bool, genStmt string,
	output string, addNode int, cpuQuota int, memQuota int64, allowUnpin bool) error {

	resize := false
	if plan == nil {
		resize = true
	}

	config := DefaultRunConfig()
	config.Detail = detail
	config.GenStmt = genStmt
	config.Resize = resize
	config.Output = output
	config.AddNode = addNode
	config.MemQuota = memQuota
	config.CpuQuota = cpuQuota
	config.AllowUnpin = allowUnpin

	p, _, err := execute(config, CommandPlan, plan, indexSpecs, ([]string)(nil))
	if p != nil && detail {
		logging.Infof("************ Indexer Layout *************")
		p.PrintLayout()
		logging.Infof("****************************************")
	}

	return err
}

func ExecuteRebalanceWithOptions(plan *Plan, indexSpecs []*IndexSpec, detail bool, genStmt string,
	output string, addNode int, cpuQuota int, memQuota int64, allowUnpin bool, deletedNodes []string) error {

	config := DefaultRunConfig()
	config.Detail = detail
	config.GenStmt = genStmt
	config.Resize = false
	config.Output = output
	config.AddNode = addNode
	config.MemQuota = memQuota
	config.CpuQuota = cpuQuota
	config.AllowUnpin = allowUnpin

	p, _, err := execute(config, CommandRebalance, plan, indexSpecs, deletedNodes)

	if detail {
		logging.Infof("************ Indexer Layout *************")
		p.PrintLayout()
		logging.Infof("****************************************")
	}

	return err
}

func execute(config *RunConfig, command CommandType, p *Plan, indexSpecs []*IndexSpec, deletedNodes []string) (*SAPlanner, *RunStats, error) {

	var indexes []*IndexUsage
	var err error

	sizing := newMOISizingMethod()

	if command == CommandPlan {
		if indexSpecs != nil {
			indexes, err = indexUsagesFromSpec(sizing, indexSpecs)
			if err != nil {
				return nil, nil, err
			}
		} else {
			return nil, nil, errors.New("missing argument: index spec must be present")
		}

		return plan(config, p, indexes)

	} else if command == CommandRebalance {
		if plan == nil {
			return nil, nil, errors.New("missing argument: either workload or plan must be present")
		}

		return rebalance(config, p, indexes, deletedNodes)

	} else {
		panic(fmt.Sprintf("uknown command: %v", command))
	}

	return nil, nil, nil
}

func plan(config *RunConfig, plan *Plan, indexes []*IndexUsage) (*SAPlanner, *RunStats, error) {

	var constraint ConstraintMethod
	var sizing SizingMethod
	var placement PlacementMethod
	var cost CostMethod

	var solution *Solution
	var initialIndexes []*IndexUsage

	sizing = newMOISizingMethod()

	// update runtime stats
	s := &RunStats{}
	setIndexPlacementStats(s, indexes, false)

	// create a solution
	if plan != nil {
		// create a solution from plan
		var movedIndex, movedData uint64
		solution, constraint, initialIndexes, movedIndex, movedData = solutionFromPlan(CommandPlan, config, sizing, plan)
		setInitialLayoutStats(s, config, constraint, solution, initialIndexes, movedIndex, movedData, false)

	} else {
		// create an empty solution
		solution, constraint = emptySolution(config, sizing, indexes)
	}

	// create placement method
	if plan != nil && config.AllowMove {
		// incremental placement with move
		total := ([]*IndexUsage)(nil)
		total = append(total, initialIndexes...)
		total = append(total, indexes...)
		total = filterPinnedIndexes(config, total)
		placement = newRandomPlacement(total, config.AllowSwap)
	} else {
		// initial placement
		indexes = filterPinnedIndexes(config, indexes)
		placement = newRandomPlacement(indexes, config.AllowSwap)
	}
	placement.Add(solution, indexes)

	// run planner
	cost = newUsageBasedCostMethod(constraint, config.DataCostWeight, config.CpuCostWeight, config.MemCostWeight)
	planner := newSAPlanner(cost, constraint, placement, sizing)
	if _, err := planner.Plan(CommandPlan, solution); err != nil {
		return planner, s, err
	}

	// save result
	s.MemoryQuota = constraint.GetMemQuota()
	s.CpuQuota = constraint.GetCpuQuota()

	if config.Output != "" {
		if err := savePlan(config.Output, planner.Result, constraint); err != nil {
			return nil, nil, err
		}
	}

	if config.GenStmt != "" {
		if err := genCreateIndexDDL(config.GenStmt, planner.Result); err != nil {
			return nil, nil, err
		}
	}

	return planner, s, nil
}

func rebalance(config *RunConfig, plan *Plan, indexes []*IndexUsage, deletedNodes []string) (*SAPlanner, *RunStats, error) {

	var constraint ConstraintMethod
	var sizing SizingMethod
	var placement PlacementMethod
	var cost CostMethod

	var solution *Solution
	var initialIndexes []*IndexUsage
	var outIndexes []*IndexUsage

	var err error

	s := &RunStats{}

	sizing = newMOISizingMethod()

	// create an initial solution
	if plan != nil {
		// create an initial solution from plan
		var movedIndex, movedData uint64
		solution, constraint, initialIndexes, movedIndex, movedData = solutionFromPlan(CommandRebalance, config, sizing, plan)
		setInitialLayoutStats(s, config, constraint, solution, initialIndexes, movedIndex, movedData, true)

	} else {
		// create an initial solution
		initialIndexes = indexes
		solution, constraint = initialSolution(config, sizing, initialIndexes)
		setInitialLayoutStats(s, config, constraint, solution, initialIndexes, 0, 0, false)
	}

	// change topology before rebalancing
	outIndexes, err = changeTopology(config, solution, deletedNodes)
	if err != nil {
		return nil, nil, err
	}

	// create placement method
	if len(outIndexes) != 0 {
		indexes = outIndexes
	} else if !config.EjectOnly {
		indexes = initialIndexes
	} else {
		indexes = nil
	}
	placement = newRandomPlacement(indexes, config.AllowSwap)

	// run planner
	cost = newUsageBasedCostMethod(constraint, config.DataCostWeight, config.CpuCostWeight, config.MemCostWeight)
	planner := newSAPlanner(cost, constraint, placement, sizing)
	if _, err := planner.Plan(CommandRebalance, solution); err != nil {
		return planner, s, err
	}

	// save result
	s.MemoryQuota = constraint.GetMemQuota()
	s.CpuQuota = constraint.GetCpuQuota()

	if config.Output != "" {
		if err := savePlan(config.Output, planner.Result, constraint); err != nil {
			return nil, nil, err
		}
	}

	return planner, s, nil
}

//////////////////////////////////////////////////////////////
// Generate DDL
/////////////////////////////////////////////////////////////

//
// CREATE INDEX [index_name] ON named_keyspace_ref( expression [ , expression ] * )
//   WHERE filter_expressions
//   [ USING GSI | VIEW ]
//   [ WITH { "nodes": [ "node_name" ], "defer_build":true|false } ];
// BUILD INDEX ON named_keyspace_ref(index_name[,index_name]*) USING GSI;
//
func genCreateIndexDDL(ddl string, solution *Solution) error {

	if solution == nil || ddl == "" {
		return nil
	}

	var allStmts string

	for _, indexer := range solution.Placement {

		buckets := make(map[string][]*IndexUsage)

		for _, index := range indexer.Indexes {
			if index.initialNode == nil && index.Definition != nil {
				buckets[index.Definition.Bucket] = append(buckets[index.Definition.Bucket], index)
			}
		}

		for bucket, indexes := range buckets {
			var stmts string

			for _, index := range indexes {
				index.Definition.Nodes = make([]string, 1)
				index.Definition.Nodes[0] = indexer.NodeId
				index.Definition.Deferred = true

				stmt := common.IndexStatement(*index.Definition) + "\n"

				stmts += stmt
			}

			buildStmt := fmt.Sprintf("BUILD INDEX ON %v(", bucket)
			for i := 0; i < len(indexes); i++ {
				buildStmt += indexes[i].Definition.Name
				if i < len(indexes)-1 {
					buildStmt += ","
				}
			}
			buildStmt += ")\n"

			allStmts += stmts + buildStmt
			allStmts += "\n"
		}
	}

	if err := ioutil.WriteFile(ddl, ([]byte)(allStmts), os.ModePerm); err != nil {
		return errors.New(fmt.Sprintf("Unable to write DDL statements into %v. err = %s", ddl, err))
	}

	return nil
}

//////////////////////////////////////////////////////////////
// RunConfig
/////////////////////////////////////////////////////////////

func DefaultRunConfig() *RunConfig {

	return &RunConfig{
		Detail:         false,
		GenStmt:        "",
		MemQuotaFactor: 1.0,
		CpuQuotaFactor: 1.0,
		Resize:         true,
		MaxNumNode:     int(math.MaxInt16),
		Output:         "",
		Shuffle:        0,
		AllowMove:      false,
		AllowSwap:      true,
		AllowUnpin:     false,
		AddNode:        0,
		DeleteNode:     0,
		MaxMemUse:      -1,
		MaxCpuUse:      -1,
		MemQuota:       -1,
		CpuQuota:       -1,
		DataCostWeight: 1,
		CpuCostWeight:  1,
		MemCostWeight:  1,
		EjectOnly:      false,
	}
}

//////////////////////////////////////////////////////////////
// Indexer Nodes Generation
/////////////////////////////////////////////////////////////

func indexerNodes(constraint ConstraintMethod, indexes []*IndexUsage, sizing SizingMethod, useLive bool) []*IndexerNode {

	rs := rand.New(rand.NewSource(time.Now().UnixNano()))

	quota := constraint.GetMemQuota()

	total := uint64(0)
	for _, index := range indexes {
		total += index.GetMemTotal(useLive)
	}

	numOfIndexers := int(total / quota)
	var indexers []*IndexerNode
	for i := 0; i < numOfIndexers; i++ {
		nodeId := strconv.FormatUint(uint64(rs.Uint32()), 10)
		indexers = append(indexers, newIndexerNode(nodeId, sizing))
	}

	return indexers
}

func indexerNode(rs *rand.Rand, prefix string, sizing SizingMethod) *IndexerNode {
	nodeId := strconv.FormatUint(uint64(rs.Uint32()), 10)
	return newIndexerNode(prefix+nodeId, sizing)
}

//////////////////////////////////////////////////////////////
// Solution Generation
/////////////////////////////////////////////////////////////

func initialSolution(config *RunConfig,
	sizing SizingMethod,
	indexes []*IndexUsage) (*Solution, ConstraintMethod) {

	resize := config.Resize
	maxNumNode := config.MaxNumNode
	maxCpuUse := config.MaxCpuUse
	maxMemUse := config.MaxMemUse

	memQuota, cpuQuota := computeQuota(config, sizing, indexes, false)

	constraint := newIndexerConstraint(memQuota, cpuQuota, resize, maxNumNode, maxCpuUse, maxMemUse)

	indexers := indexerNodes(constraint, indexes, sizing, false)

	r := newSolution(constraint, sizing, indexers, false, false)

	placement := newRandomPlacement(indexes, config.AllowSwap)
	placement.InitialPlace(r, indexes)

	return r, constraint
}

func emptySolution(config *RunConfig,
	sizing SizingMethod,
	indexes []*IndexUsage) (*Solution, ConstraintMethod) {

	resize := config.Resize
	maxNumNode := config.MaxNumNode
	maxCpuUse := config.MaxCpuUse
	maxMemUse := config.MaxMemUse

	memQuota, cpuQuota := computeQuota(config, sizing, indexes, false)

	constraint := newIndexerConstraint(memQuota, cpuQuota, resize, maxNumNode, maxCpuUse, maxMemUse)

	r := newSolution(constraint, sizing, ([]*IndexerNode)(nil), false, false)

	return r, constraint
}

func solutionFromPlan(command CommandType, config *RunConfig, sizing SizingMethod,
	plan *Plan) (*Solution, ConstraintMethod, []*IndexUsage, uint64, uint64) {

	memQuotaFactor := config.MemQuotaFactor
	cpuQuotaFactor := config.CpuQuotaFactor
	resize := config.Resize
	maxNumNode := config.MaxNumNode
	shuffle := config.Shuffle
	maxCpuUse := config.MaxCpuUse
	maxMemUse := config.MaxMemUse

	movedData := uint64(0)
	movedIndex := uint64(0)

	indexes := ([]*IndexUsage)(nil)

	for _, indexer := range plan.Placement {
		for _, index := range indexer.Indexes {
			index.initialNode = indexer
			indexes = append(indexes, index)
		}
	}

	memQuota, cpuQuota := computeQuota(config, sizing, indexes, plan.IsLive && command == CommandRebalance)

	if config.MemQuota == -1 && plan.MemQuota != 0 {
		memQuota = uint64(float64(plan.MemQuota) * memQuotaFactor)
	}

	if config.CpuQuota == -1 && plan.CpuQuota != 0 {
		cpuQuota = uint64(float64(plan.CpuQuota) * cpuQuotaFactor)
	}

	constraint := newIndexerConstraint(memQuota, cpuQuota, resize, maxNumNode, maxCpuUse, maxMemUse)

	r := newSolution(constraint, sizing, plan.Placement, plan.IsLive, command == CommandRebalance)
	r.calculateSize() // in case sizing formula changes

	if shuffle != 0 {
		placement := newRandomPlacement(indexes, config.AllowSwap)
		movedIndex, movedData = placement.randomMoveNoConstraint(r, shuffle)
	}

	for _, indexer := range r.Placement {
		for _, index := range indexer.Indexes {
			index.initialNode = indexer
		}
	}

	return r, constraint, indexes, movedIndex, movedData
}

func computeQuota(config *RunConfig, sizing SizingMethod, indexes []*IndexUsage, useLive bool) (uint64, uint64) {

	memQuotaFactor := config.MemQuotaFactor
	cpuQuotaFactor := config.CpuQuotaFactor

	memQuota := uint64(config.MemQuota)
	cpuQuota := uint64(config.CpuQuota)

	imemQuota, icpuQuota := sizing.ComputeMinQuota(indexes, useLive)

	if config.MemQuota == -1 {
		memQuota = imemQuota
	}
	memQuota = uint64(float64(memQuota) * memQuotaFactor)

	if config.CpuQuota == -1 {
		cpuQuota = icpuQuota
	}
	cpuQuota = uint64(float64(cpuQuota) * cpuQuotaFactor)

	return memQuota, cpuQuota
}

func filterPinnedIndexes(config *RunConfig, indexes []*IndexUsage) []*IndexUsage {

	result := ([]*IndexUsage)(nil)
	for _, index := range indexes {

		if config.AllowUnpin || (index.initialNode != nil && index.initialNode.delete) {
			index.Hosts = nil
		}

		if len(index.Hosts) == 0 {
			result = append(result, index)
		}
	}

	return result
}

//////////////////////////////////////////////////////////////
// Topology Change
/////////////////////////////////////////////////////////////

func changeTopology(config *RunConfig, solution *Solution, deletedNodes []string) ([]*IndexUsage, error) {

	outNodes := ([]*IndexerNode)(nil)
	outNodeIds := ([]string)(nil)
	inNodeIds := ([]string)(nil)
	outIndexes := ([]*IndexUsage)(nil)

	if len(deletedNodes) != 0 {

		if len(deletedNodes) > len(solution.Placement) {
			return nil, errors.New("The number of node in cluster is smaller than the number of node to be deleted.")
		}

		for _, nodeId := range deletedNodes {

			candidate := solution.findMatchingIndexer(nodeId)
			if candidate == nil {
				return nil, errors.New(fmt.Sprintf("Cannot find to-be-deleted indexer in solution: %v", nodeId))
			}

			candidate.delete = true
			outNodes = append(outNodes, candidate)
		}

		for _, outNode := range outNodes {
			outNodeIds = append(outNodeIds, outNode.String())
			for _, index := range outNode.Indexes {
				outIndexes = append(outIndexes, index)
			}
		}

		logging.Tracef("Nodes to be removed : %v", outNodeIds)
	}

	if config.AddNode != 0 {
		rs := rand.New(rand.NewSource(time.Now().UnixNano()))

		for i := 0; i < config.AddNode; i++ {
			indexer := indexerNode(rs, "newNode-", solution.getSizingMethod())
			solution.Placement = append(solution.Placement, indexer)
			inNodeIds = append(inNodeIds, indexer.String())
		}
		logging.Tracef("Nodes to be added: %v", inNodeIds)
	}

	return outIndexes, nil
}

//////////////////////////////////////////////////////////////
// RunStats
/////////////////////////////////////////////////////////////

//
// Set stats for index to be placed
//
func setIndexPlacementStats(s *RunStats, indexes []*IndexUsage, useLive bool) {
	s.AvgIndexSize, s.StdDevIndexSize = computeIndexMemStats(indexes, useLive)
	s.AvgIndexCpu, s.StdDevIndexCpu = computeIndexCpuStats(indexes)
	s.IndexCount = uint64(len(indexes))
}

//
// Set stats for initial layout
//
func setInitialLayoutStats(s *RunStats,
	config *RunConfig,
	constraint ConstraintMethod,
	solution *Solution,
	initialIndexes []*IndexUsage,
	movedIndex uint64,
	movedData uint64,
	useLive bool) {

	s.Initial_avgIndexerSize, s.Initial_stdDevIndexerSize = solution.ComputeMemUsage()
	s.Initial_avgIndexerCpu, s.Initial_stdDevIndexerCpu = solution.ComputeCpuUsage()
	s.Initial_avgIndexSize, s.Initial_stdDevIndexSize = computeIndexMemStats(initialIndexes, useLive)
	s.Initial_avgIndexCpu, s.Initial_stdDevIndexCpu = computeIndexCpuStats(initialIndexes)
	s.Initial_indexCount = uint64(len(initialIndexes))
	s.Initial_indexerCount = uint64(len(solution.Placement))

	initial_cost := newUsageBasedCostMethod(constraint, config.DataCostWeight, config.CpuCostWeight, config.MemCostWeight)
	s.Initial_score = initial_cost.Cost(solution)

	s.Initial_movedIndex = movedIndex
	s.Initial_movedData = movedData
}

//////////////////////////////////////////////////////////////
// Index Generation (from Index Spec)
/////////////////////////////////////////////////////////////

func indexUsagesFromSpec(sizing SizingMethod, specs []*IndexSpec) ([]*IndexUsage, error) {

	var indexes []*IndexUsage
	for _, spec := range specs {
		usages, err := indexUsageFromSpec(sizing, spec)
		if err != nil {
			return nil, err
		}
		indexes = append(indexes, usages...)
	}

	return indexes, nil
}

func indexUsageFromSpec(sizing SizingMethod, spec *IndexSpec) ([]*IndexUsage, error) {

	result := make([]*IndexUsage, spec.Replica)

	uuid, err := common.NewUUID()
	if err != nil {
		return nil, errors.New("unable to generate UUID")
	}

	for i := 0; i < int(spec.Replica); i++ {
		index := &IndexUsage{}
		index.DefnId = common.IndexDefnId(uuid.Uint64())
		index.InstId = common.IndexInstId(i)
		index.Bucket = spec.Bucket
		index.IsMOI = true
		index.IsPrimary = spec.IsPrimary

		if i == 0 {
			index.Name = spec.Name
		} else {
			index.Name = fmt.Sprintf("%v_%v", spec.Name, i)
		}

		index.Definition = &common.IndexDefn{}
		index.Definition.Name = index.Name
		index.Definition.Bucket = spec.Bucket
		index.Definition.IsPrimary = spec.IsPrimary
		index.Definition.SecExprs = spec.SecExprs
		index.Definition.WhereExpr = spec.WhereExpr
		index.Definition.Immutable = spec.Immutable
		index.Definition.IsArrayIndex = spec.IsArrayIndex

		index.NumOfDocs = spec.NumDoc
		index.AvgDocKeySize = spec.DocKeySize
		index.AvgSecKeySize = spec.SecKeySize
		index.AvgArrKeySize = spec.ArrKeySize
		index.AvgArrSize = spec.ArrSize
		index.MutationRate = spec.MutationRate
		index.ScanRate = spec.ScanRate

		sizing.ComputeIndexSize(index)

		result[i] = index
	}

	return result, nil
}

//////////////////////////////////////////////////////////////
// Plan
/////////////////////////////////////////////////////////////

func printPlanSummary(plan *Plan) {

	if plan == nil {
		return
	}

	logging.Infof("--------------------------------------")
	logging.Infof("Mem Quota:	%v", formatMemoryStr(plan.MemQuota))
	logging.Infof("Cpu Quota:	%v", plan.CpuQuota)
	logging.Infof("--------------------------------------")
}

func savePlan(output string, solution *Solution, constraint ConstraintMethod) error {

	plan := &Plan{
		Placement: solution.Placement,
		MemQuota:  constraint.GetMemQuota(),
		CpuQuota:  constraint.GetCpuQuota(),
		IsLive:    solution.isLiveData,
	}

	data, err := json.MarshalIndent(plan, "", "	")
	if err != nil {
		return errors.New(fmt.Sprintf("Unable to save plan into %v. err = %s", output, err))
	}

	err = ioutil.WriteFile(output, data, os.ModePerm)
	if err != nil {
		return errors.New(fmt.Sprintf("Unable to save plan into %v. err = %s", output, err))
	}

	return nil
}

func ReadPlan(planFile string) (*Plan, error) {

	if planFile != "" {

		plan := &Plan{}

		buf, err := ioutil.ReadFile(planFile)
		if err != nil {
			return nil, errors.New(fmt.Sprintf("Unable to read plan from %v. err = %s", planFile, err))
		}

		if err := json.Unmarshal(buf, plan); err != nil {
			return nil, errors.New(fmt.Sprintf("Unable to parse plan from %v. err = %s", planFile, err))
		}

		return plan, nil
	}

	return nil, nil
}

//////////////////////////////////////////////////////////////
// IndexSpec
/////////////////////////////////////////////////////////////

func ReadIndexSpecs(specFile string) ([]*IndexSpec, error) {

	if specFile != "" {

		var specs []*IndexSpec

		buf, err := ioutil.ReadFile(specFile)
		if err != nil {
			return nil, errors.New(fmt.Sprintf("Unable to read index spec from %v. err = %s", specFile, err))
		}

		if err := json.Unmarshal(buf, &specs); err != nil {
			return nil, errors.New(fmt.Sprintf("Unable to parse index spec from %v. err = %s", specFile, err))
		}

		return specs, nil
	}

	return nil, nil
}
