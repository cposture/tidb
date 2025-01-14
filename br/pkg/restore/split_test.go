// Copyright 2020 PingCAP, Inc. Licensed under Apache-2.0.

package restore

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pingcap/errors"
	backuppb "github.com/pingcap/kvproto/pkg/brpb"
	"github.com/pingcap/kvproto/pkg/import_sstpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/log"
	berrors "github.com/pingcap/tidb/br/pkg/errors"
	"github.com/pingcap/tidb/br/pkg/glue"
	"github.com/pingcap/tidb/br/pkg/logutil"
	"github.com/pingcap/tidb/br/pkg/restore/split"
	"github.com/pingcap/tidb/br/pkg/rtree"
	"github.com/pingcap/tidb/br/pkg/utils/iter"
	"github.com/pingcap/tidb/pkg/parser/model"
	"github.com/pingcap/tidb/pkg/store/pdtypes"
	"github.com/pingcap/tidb/pkg/tablecodec"
	"github.com/pingcap/tidb/pkg/util/codec"
	"github.com/stretchr/testify/require"
	"go.uber.org/multierr"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type TestClient struct {
	split.SplitClient

	mu               sync.RWMutex
	stores           map[uint64]*metapb.Store
	regions          map[uint64]*split.RegionInfo
	regionsInfo      *pdtypes.RegionTree // For now it's only used in ScanRegions
	nextRegionID     uint64
	injectInScatter  func(*split.RegionInfo) error
	injectInOperator func(uint64) (*pdpb.GetOperatorResponse, error)

	scattered   map[uint64]bool
	InjectErr   bool
	InjectTimes int32
}

func NewTestClient(
	stores map[uint64]*metapb.Store,
	regions map[uint64]*split.RegionInfo,
	nextRegionID uint64,
) *TestClient {
	regionsInfo := &pdtypes.RegionTree{}
	for _, regionInfo := range regions {
		regionsInfo.SetRegion(pdtypes.NewRegionInfo(regionInfo.Region, regionInfo.Leader))
	}
	return &TestClient{
		stores:          stores,
		regions:         regions,
		regionsInfo:     regionsInfo,
		nextRegionID:    nextRegionID,
		scattered:       map[uint64]bool{},
		injectInScatter: func(*split.RegionInfo) error { return nil },
	}
}

// ScatterRegions scatters regions in a batch.
func (c *TestClient) ScatterRegions(ctx context.Context, regionInfo []*split.RegionInfo) error {
	regions := map[uint64]*split.RegionInfo{}
	for _, region := range regionInfo {
		regions[region.Region.Id] = region
	}
	var err error
	for i := 0; i < 3; i++ {
		if len(regions) == 0 {
			return nil
		}
		for id, region := range regions {
			splitErr := c.ScatterRegion(ctx, region)
			if splitErr == nil {
				delete(regions, id)
			}
			err = multierr.Append(err, splitErr)
		}
	}
	return nil
}

func (c *TestClient) GetAllRegions() map[uint64]*split.RegionInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.regions
}

func (c *TestClient) GetStore(ctx context.Context, storeID uint64) (*metapb.Store, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	store, ok := c.stores[storeID]
	if !ok {
		return nil, errors.Errorf("store not found")
	}
	return store, nil
}

func (c *TestClient) GetRegion(ctx context.Context, key []byte) (*split.RegionInfo, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, region := range c.regions {
		if bytes.Compare(key, region.Region.StartKey) >= 0 &&
			(len(region.Region.EndKey) == 0 || bytes.Compare(key, region.Region.EndKey) < 0) {
			return region, nil
		}
	}
	return nil, errors.Errorf("region not found: key=%s", string(key))
}

func (c *TestClient) GetRegionByID(ctx context.Context, regionID uint64) (*split.RegionInfo, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	region, ok := c.regions[regionID]
	if !ok {
		return nil, errors.Errorf("region not found: id=%d", regionID)
	}
	return region, nil
}

func (c *TestClient) SplitRegion(
	ctx context.Context,
	regionInfo *split.RegionInfo,
	key []byte,
) (*split.RegionInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var target *split.RegionInfo
	splitKey := codec.EncodeBytes([]byte{}, key)
	for _, region := range c.regions {
		if bytes.Compare(splitKey, region.Region.StartKey) >= 0 &&
			(len(region.Region.EndKey) == 0 || bytes.Compare(splitKey, region.Region.EndKey) < 0) {
			target = region
		}
	}
	if target == nil {
		return nil, errors.Errorf("region not found: key=%s", string(key))
	}
	newRegion := &split.RegionInfo{
		Region: &metapb.Region{
			Peers:    target.Region.Peers,
			Id:       c.nextRegionID,
			StartKey: target.Region.StartKey,
			EndKey:   splitKey,
		},
	}
	c.regions[c.nextRegionID] = newRegion
	c.nextRegionID++
	target.Region.StartKey = splitKey
	c.regions[target.Region.Id] = target
	return newRegion, nil
}

func (c *TestClient) BatchSplitRegionsWithOrigin(
	ctx context.Context, regionInfo *split.RegionInfo, keys [][]byte,
) (*split.RegionInfo, []*split.RegionInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	newRegions := make([]*split.RegionInfo, 0)
	var region *split.RegionInfo
	for _, key := range keys {
		var target *split.RegionInfo
		splitKey := codec.EncodeBytes([]byte{}, key)
		for _, region := range c.regions {
			if region.ContainsInterior(splitKey) {
				target = region
			}
		}
		if target == nil {
			continue
		}
		newRegion := &split.RegionInfo{
			Region: &metapb.Region{
				Peers:    target.Region.Peers,
				Id:       c.nextRegionID,
				StartKey: target.Region.StartKey,
				EndKey:   splitKey,
			},
		}
		c.regions[c.nextRegionID] = newRegion
		c.nextRegionID++
		target.Region.StartKey = splitKey
		c.regions[target.Region.Id] = target
		region = target
		newRegions = append(newRegions, newRegion)
	}
	return region, newRegions, nil
}

func (c *TestClient) BatchSplitRegions(
	ctx context.Context, regionInfo *split.RegionInfo, keys [][]byte,
) ([]*split.RegionInfo, error) {
	_, newRegions, err := c.BatchSplitRegionsWithOrigin(ctx, regionInfo, keys)
	return newRegions, err
}

func (c *TestClient) ScatterRegion(ctx context.Context, regionInfo *split.RegionInfo) error {
	return c.injectInScatter(regionInfo)
}

func (c *TestClient) GetOperator(ctx context.Context, regionID uint64) (*pdpb.GetOperatorResponse, error) {
	if c.injectInOperator != nil {
		return c.injectInOperator(regionID)
	}
	return &pdpb.GetOperatorResponse{
		Header: new(pdpb.ResponseHeader),
	}, nil
}

func (c *TestClient) ScanRegions(ctx context.Context, key, endKey []byte, limit int) ([]*split.RegionInfo, error) {
	if c.InjectErr && c.InjectTimes > 0 {
		c.InjectTimes -= 1
		return nil, status.Error(codes.Unavailable, "not leader")
	}
	if len(key) != 0 && bytes.Equal(key, endKey) {
		return nil, status.Error(codes.Internal, "key and endKey are the same")
	}

	infos := c.regionsInfo.ScanRange(key, endKey, limit)
	regions := make([]*split.RegionInfo, 0, len(infos))
	for _, info := range infos {
		regions = append(regions, &split.RegionInfo{
			Region: info.Meta,
			Leader: info.Leader,
		})
	}
	return regions, nil
}

func (c *TestClient) IsScatterRegionFinished(
	ctx context.Context,
	regionID uint64,
) (scatterDone bool, needRescatter bool, scatterErr error) {
	resp, _ := c.GetOperator(ctx, regionID)
	return split.IsScatterRegionFinished(resp)
}

func TestScanEmptyRegion(t *testing.T) {
	client := initTestClient(false)
	ranges := initRanges()
	// make ranges has only one
	ranges = ranges[0:1]
	rewriteRules := initRewriteRules()
	regionSplitter := NewRegionSplitter(client)

	ctx := context.Background()
	err := regionSplitter.ExecuteSplit(ctx, ranges, rewriteRules, 1, false, func(key [][]byte) {})
	// should not return error with only one range entry
	require.NoError(t, err)
}

// region: [, aay), [aay, bba), [bba, bbh), [bbh, cca), [cca, )
// range: [aaa, aae), [aae, aaz), [ccd, ccf), [ccf, ccj)
// rewrite rules: aa -> xx,  cc -> bb
// expected regions after split:
//
//	[, aay), [aay, bba), [bba, bbf), [bbf, bbh), [bbh, bbj),
//	[bbj, cca), [cca, xxe), [xxe, xxz), [xxz, )
func TestSplitAndScatter(t *testing.T) {
	t.Run("BatchScatter", func(t *testing.T) {
		client := initTestClient(false)
		runTestSplitAndScatterWith(t, client)
	})
	t.Run("WaitScatter", func(t *testing.T) {
		client := initTestClient(false)
		runWaitScatter(t, client)
	})
}

// +------------+----------------------------
// |   region   | states
// +------------+----------------------------
// | [   , aay) | SUCCESS
// +------------+----------------------------
// | [aay, bba) | CANCEL, SUCCESS
// +------------+----------------------------
// | [bba, bbh) | RUNNING, TIMEOUT, SUCCESS
// +------------+----------------------------
// | [bbh, cca) | <NOT_SCATTER_OPEARTOR>
// +------------+----------------------------
// | [cca,    ) | CANCEL, RUNNING, SUCCESS
// +------------+----------------------------
// region: [, aay), [aay, bba), [bba, bbh), [bbh, cca), [cca, )
// states:
func runWaitScatter(t *testing.T, client *TestClient) {
	// configuration
	type Operatorstates struct {
		index  int
		status []pdpb.OperatorStatus
	}
	results := map[string]*Operatorstates{
		"": {status: []pdpb.OperatorStatus{pdpb.OperatorStatus_SUCCESS}},
		string(codec.EncodeBytesExt([]byte{}, []byte("aay"), false)): {status: []pdpb.OperatorStatus{pdpb.OperatorStatus_CANCEL, pdpb.OperatorStatus_SUCCESS}},
		string(codec.EncodeBytesExt([]byte{}, []byte("bba"), false)): {status: []pdpb.OperatorStatus{pdpb.OperatorStatus_RUNNING, pdpb.OperatorStatus_TIMEOUT, pdpb.OperatorStatus_SUCCESS}},
		string(codec.EncodeBytesExt([]byte{}, []byte("bbh"), false)): {},
		string(codec.EncodeBytesExt([]byte{}, []byte("cca"), false)): {status: []pdpb.OperatorStatus{pdpb.OperatorStatus_CANCEL, pdpb.OperatorStatus_RUNNING, pdpb.OperatorStatus_SUCCESS}},
	}
	// after test done, the `leftScatterCount` should be empty
	leftScatterCount := map[string]int{
		string(codec.EncodeBytesExt([]byte{}, []byte("aay"), false)): 1,
		string(codec.EncodeBytesExt([]byte{}, []byte("bba"), false)): 1,
		string(codec.EncodeBytesExt([]byte{}, []byte("cca"), false)): 1,
	}
	client.injectInScatter = func(ri *split.RegionInfo) error {
		states, ok := results[string(ri.Region.StartKey)]
		require.True(t, ok)
		require.NotEqual(t, 0, len(states.status))
		require.NotEqual(t, pdpb.OperatorStatus_SUCCESS, states.status[states.index])
		states.index += 1
		cnt, ok := leftScatterCount[string(ri.Region.StartKey)]
		require.True(t, ok)
		if cnt == 1 {
			delete(leftScatterCount, string(ri.Region.StartKey))
		} else {
			leftScatterCount[string(ri.Region.StartKey)] = cnt - 1
		}
		return nil
	}
	regionsMap := client.GetAllRegions()
	leftOperatorCount := map[string]int{
		"": 1,
		string(codec.EncodeBytesExt([]byte{}, []byte("aay"), false)): 2,
		string(codec.EncodeBytesExt([]byte{}, []byte("bba"), false)): 3,
		string(codec.EncodeBytesExt([]byte{}, []byte("bbh"), false)): 1,
		string(codec.EncodeBytesExt([]byte{}, []byte("cca"), false)): 3,
	}
	client.injectInOperator = func(u uint64) (*pdpb.GetOperatorResponse, error) {
		ri := regionsMap[u]
		cnt, ok := leftOperatorCount[string(ri.Region.StartKey)]
		require.True(t, ok)
		if cnt == 1 {
			delete(leftOperatorCount, string(ri.Region.StartKey))
		} else {
			leftOperatorCount[string(ri.Region.StartKey)] = cnt - 1
		}
		states, ok := results[string(ri.Region.StartKey)]
		require.True(t, ok)
		if len(states.status) == 0 {
			return &pdpb.GetOperatorResponse{
				Desc: []byte("other"),
			}, nil
		}
		if states.status[states.index] == pdpb.OperatorStatus_RUNNING {
			states.index += 1
			return &pdpb.GetOperatorResponse{
				Desc:   []byte("scatter-region"),
				Status: states.status[states.index-1],
			}, nil
		}
		return &pdpb.GetOperatorResponse{
			Desc:   []byte("scatter-region"),
			Status: states.status[states.index],
		}, nil
	}

	// begin to test
	ctx := context.Background()
	regions := make([]*split.RegionInfo, 0, len(regionsMap))
	for _, info := range regionsMap {
		regions = append(regions, info)
	}
	regionSplitter := NewRegionSplitter(client)
	leftCnt := regionSplitter.WaitForScatterRegionsTimeout(ctx, regions, 2000*time.Second)
	require.Equal(t, leftCnt, 0)
}

func runTestSplitAndScatterWith(t *testing.T, client *TestClient) {
	ranges := initRanges()
	rewriteRules := initRewriteRules()
	regionSplitter := NewRegionSplitter(client)

	ctx := context.Background()
	err := regionSplitter.ExecuteSplit(ctx, ranges, rewriteRules, 1, false, func(key [][]byte) {})
	require.NoError(t, err)
	regions := client.GetAllRegions()
	if !validateRegions(regions) {
		for _, region := range regions {
			t.Logf("region: %v\n", region.Region)
		}
		t.Log("get wrong result")
		t.Fail()
	}
	regionInfos := make([]*split.RegionInfo, 0, len(regions))
	for _, info := range regions {
		regionInfos = append(regionInfos, info)
	}
	scattered := map[uint64]bool{}
	const alwaysFailedRegionID = 1
	client.injectInScatter = func(regionInfo *split.RegionInfo) error {
		if _, ok := scattered[regionInfo.Region.Id]; !ok || regionInfo.Region.Id == alwaysFailedRegionID {
			scattered[regionInfo.Region.Id] = false
			return status.Errorf(codes.Unknown, "region %d is not fully replicated", regionInfo.Region.Id)
		}
		scattered[regionInfo.Region.Id] = true
		return nil
	}
	err = regionSplitter.client.ScatterRegions(ctx, regionInfos)
	require.NoError(t, err)
	for key := range regions {
		if key == alwaysFailedRegionID {
			require.Falsef(t, scattered[key], "always failed region %d was scattered successfully", key)
		} else if !scattered[key] {
			t.Fatalf("region %d has not been scattered: %#v", key, regions[key])
		}
	}
}

func TestRawSplit(t *testing.T) {
	// Fix issue #36490.
	ranges := []rtree.Range{
		{
			StartKey: []byte{0},
			EndKey:   []byte{},
		},
	}
	client := initTestClient(true)
	ctx := context.Background()

	regionSplitter := NewRegionSplitter(client)
	err := regionSplitter.ExecuteSplit(ctx, ranges, nil, 1, true, func(key [][]byte) {})
	require.NoError(t, err)
	regions := client.GetAllRegions()
	expectedKeys := []string{"", "aay", "bba", "bbh", "cca", ""}
	if !validateRegionsExt(regions, expectedKeys, true) {
		for _, region := range regions {
			t.Logf("region: %v\n", region.Region)
		}
		t.Log("get wrong result")
		t.Fail()
	}
}

// region: [, aay), [aay, bba), [bba, bbh), [bbh, cca), [cca, )
func initTestClient(isRawKv bool) *TestClient {
	peers := make([]*metapb.Peer, 1)
	peers[0] = &metapb.Peer{
		Id:      1,
		StoreId: 1,
	}
	keys := [6]string{"", "aay", "bba", "bbh", "cca", ""}
	regions := make(map[uint64]*split.RegionInfo)
	for i := uint64(1); i < 6; i++ {
		startKey := []byte(keys[i-1])
		if len(startKey) != 0 {
			startKey = codec.EncodeBytesExt([]byte{}, startKey, isRawKv)
		}
		endKey := []byte(keys[i])
		if len(endKey) != 0 {
			endKey = codec.EncodeBytesExt([]byte{}, endKey, isRawKv)
		}
		regions[i] = &split.RegionInfo{
			Leader: &metapb.Peer{
				Id: i,
			},
			Region: &metapb.Region{
				Id:       i,
				Peers:    peers,
				StartKey: startKey,
				EndKey:   endKey,
			},
		}
	}
	stores := make(map[uint64]*metapb.Store)
	stores[1] = &metapb.Store{
		Id: 1,
	}
	return NewTestClient(stores, regions, 6)
}

// range: [aaa, aae), [aae, aaz), [ccd, ccf), [ccf, ccj)
func initRanges() []rtree.Range {
	var ranges [4]rtree.Range
	ranges[0] = rtree.Range{
		StartKey: []byte("aaa"),
		EndKey:   []byte("aae"),
	}
	ranges[1] = rtree.Range{
		StartKey: []byte("aae"),
		EndKey:   []byte("aaz"),
	}
	ranges[2] = rtree.Range{
		StartKey: []byte("ccd"),
		EndKey:   []byte("ccf"),
	}
	ranges[3] = rtree.Range{
		StartKey: []byte("ccf"),
		EndKey:   []byte("ccj"),
	}
	return ranges[:]
}

func initRewriteRules() *RewriteRules {
	var rules [2]*import_sstpb.RewriteRule
	rules[0] = &import_sstpb.RewriteRule{
		OldKeyPrefix: []byte("aa"),
		NewKeyPrefix: []byte("xx"),
	}
	rules[1] = &import_sstpb.RewriteRule{
		OldKeyPrefix: []byte("cc"),
		NewKeyPrefix: []byte("bb"),
	}
	return &RewriteRules{
		Data: rules[:],
	}
}

// expected regions after split:
//
//	[, aay), [aay, bba), [bba, bbf), [bbf, bbh), [bbh, bbj),
//	[bbj, cca), [cca, xxe), [xxe, xxz), [xxz, )
func validateRegions(regions map[uint64]*split.RegionInfo) bool {
	keys := [...]string{"", "aay", "bba", "bbf", "bbh", "bbj", "cca", "xxe", "xxz", ""}
	return validateRegionsExt(regions, keys[:], false)
}

func validateRegionsExt(regions map[uint64]*split.RegionInfo, expectedKeys []string, isRawKv bool) bool {
	if len(regions) != len(expectedKeys)-1 {
		return false
	}
FindRegion:
	for i := 1; i < len(expectedKeys); i++ {
		for _, region := range regions {
			startKey := []byte(expectedKeys[i-1])
			if len(startKey) != 0 {
				startKey = codec.EncodeBytesExt([]byte{}, startKey, isRawKv)
			}
			endKey := []byte(expectedKeys[i])
			if len(endKey) != 0 {
				endKey = codec.EncodeBytesExt([]byte{}, endKey, isRawKv)
			}
			if bytes.Equal(region.Region.GetStartKey(), startKey) &&
				bytes.Equal(region.Region.GetEndKey(), endKey) {
				continue FindRegion
			}
		}
		return false
	}
	return true
}

func TestRegionConsistency(t *testing.T) {
	cases := []struct {
		startKey []byte
		endKey   []byte
		err      string
		regions  []*split.RegionInfo
	}{
		{
			codec.EncodeBytes([]byte{}, []byte("a")),
			codec.EncodeBytes([]byte{}, []byte("a")),
			"scan region return empty result, startKey: (.*?), endKey: (.*?)",
			[]*split.RegionInfo{},
		},
		{
			codec.EncodeBytes([]byte{}, []byte("a")),
			codec.EncodeBytes([]byte{}, []byte("a")),
			"first region 1's startKey(.*?) > startKey(.*?)",
			[]*split.RegionInfo{
				{
					Region: &metapb.Region{
						Id:       1,
						StartKey: codec.EncodeBytes([]byte{}, []byte("b")),
						EndKey:   codec.EncodeBytes([]byte{}, []byte("d")),
					},
				},
			},
		},
		{
			codec.EncodeBytes([]byte{}, []byte("b")),
			codec.EncodeBytes([]byte{}, []byte("e")),
			"last region 100's endKey(.*?) < endKey(.*?)",
			[]*split.RegionInfo{
				{
					Region: &metapb.Region{
						Id:       100,
						StartKey: codec.EncodeBytes([]byte{}, []byte("b")),
						EndKey:   codec.EncodeBytes([]byte{}, []byte("d")),
					},
				},
			},
		},
		{
			codec.EncodeBytes([]byte{}, []byte("c")),
			codec.EncodeBytes([]byte{}, []byte("e")),
			"region 6's endKey not equal to next region 8's startKey(.*?)",
			[]*split.RegionInfo{
				{
					Region: &metapb.Region{
						Id:          6,
						StartKey:    codec.EncodeBytes([]byte{}, []byte("b")),
						EndKey:      codec.EncodeBytes([]byte{}, []byte("d")),
						RegionEpoch: nil,
					},
				},
				{
					Region: &metapb.Region{
						Id:       8,
						StartKey: codec.EncodeBytes([]byte{}, []byte("e")),
						EndKey:   codec.EncodeBytes([]byte{}, []byte("f")),
					},
				},
			},
		},
	}
	for _, ca := range cases {
		err := split.CheckRegionConsistency(ca.startKey, ca.endKey, ca.regions)
		require.Error(t, err)
		require.Regexp(t, ca.err, err.Error())
	}
}

type fakeRestorer struct {
	mu sync.Mutex

	errorInSplit        bool
	splitRanges         []rtree.Range
	restoredFiles       []*backuppb.File
	tableIDIsInsequence bool
}

func (f *fakeRestorer) SplitRanges(ctx context.Context, ranges []rtree.Range, rewriteRules *RewriteRules, updateCh glue.Progress, isRawKv bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.splitRanges = append(f.splitRanges, ranges...)
	if f.errorInSplit {
		err := errors.Annotatef(berrors.ErrRestoreSplitFailed,
			"the key space takes many efforts and finally get together, how dare you split them again... :<")
		log.Error("error happens :3", logutil.ShortError(err))
		return err
	}
	return nil
}

func (f *fakeRestorer) RestoreSSTFiles(ctx context.Context, tableIDWithFiles []TableIDWithFiles, rewriteRules *RewriteRules, updateCh glue.Progress) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if ctx.Err() != nil {
		return ctx.Err()
	}
	for i, tableIDWithFile := range tableIDWithFiles {
		if int64(i) != tableIDWithFile.TableID {
			f.tableIDIsInsequence = false
		}
		f.restoredFiles = append(f.restoredFiles, tableIDWithFile.Files...)
	}
	err := errors.Annotatef(berrors.ErrRestoreWriteAndIngest, "the files to restore are taken by a hijacker, meow :3")
	log.Error("error happens :3", logutil.ShortError(err))
	return err
}

func fakeRanges(keys ...string) (r DrainResult) {
	for i := range keys {
		if i+1 == len(keys) {
			return
		}
		r.Ranges = append(r.Ranges, rtree.Range{
			StartKey: []byte(keys[i]),
			EndKey:   []byte(keys[i+1]),
			Files:    []*backuppb.File{{Name: "fake.sst"}},
		})
		r.TableEndOffsetInRanges = append(r.TableEndOffsetInRanges, len(r.Ranges))
		r.TablesToSend = append(r.TablesToSend, CreatedTable{
			Table: &model.TableInfo{
				ID: int64(i),
			},
		})
	}
	return
}

type errorInTimeSink struct {
	ctx   context.Context
	errCh chan error
	t     *testing.T
}

func (e errorInTimeSink) EmitTables(tables ...CreatedTable) {}

func (e errorInTimeSink) EmitError(err error) {
	e.errCh <- err
}

func (e errorInTimeSink) Close() {}

func (e errorInTimeSink) Wait() {
	select {
	case <-e.ctx.Done():
		e.t.Logf("The context is canceled but no error happen")
		e.t.FailNow()
	case <-e.errCh:
	}
}

func assertErrorEmitInTime(ctx context.Context, t *testing.T) errorInTimeSink {
	errCh := make(chan error, 1)
	return errorInTimeSink{
		ctx:   ctx,
		errCh: errCh,
		t:     t,
	}
}

func TestRestoreFailed(t *testing.T) {
	ranges := []DrainResult{
		fakeRanges("aax", "abx", "abz"),
		fakeRanges("abz", "bbz", "bcy"),
		fakeRanges("bcy", "cad", "xxy"),
	}
	r := &fakeRestorer{
		tableIDIsInsequence: true,
	}
	sender, err := NewTiKVSender(context.TODO(), r, nil, 1, string(FineGrained))
	require.NoError(t, err)
	dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sink := assertErrorEmitInTime(dctx, t)
	sender.PutSink(sink)
	for _, r := range ranges {
		sender.RestoreBatch(r)
	}
	sink.Wait()
	sink.Close()
	sender.Close()
	require.GreaterOrEqual(t, len(r.restoredFiles), 1)
	require.True(t, r.tableIDIsInsequence)
}

func TestSplitFailed(t *testing.T) {
	ranges := []DrainResult{
		fakeRanges("aax", "abx", "abz"),
		fakeRanges("abz", "bbz", "bcy"),
		fakeRanges("bcy", "cad", "xxy"),
	}
	r := &fakeRestorer{errorInSplit: true, tableIDIsInsequence: true}
	sender, err := NewTiKVSender(context.TODO(), r, nil, 1, string(FineGrained))
	require.NoError(t, err)
	dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sink := assertErrorEmitInTime(dctx, t)
	sender.PutSink(sink)
	for _, r := range ranges {
		sender.RestoreBatch(r)
	}
	sink.Wait()
	sender.Close()
	require.GreaterOrEqual(t, len(r.splitRanges), 2)
	require.Len(t, r.restoredFiles, 0)
	require.True(t, r.tableIDIsInsequence)
}

func keyWithTablePrefix(tableID int64, key string) []byte {
	rawKey := append(tablecodec.GenTableRecordPrefix(tableID), []byte(key)...)
	return codec.EncodeBytes([]byte{}, rawKey)
}

func TestSplitPoint(t *testing.T) {
	ctx := context.Background()
	var oldTableID int64 = 50
	var tableID int64 = 100
	rewriteRules := &RewriteRules{
		Data: []*import_sstpb.RewriteRule{
			{
				OldKeyPrefix: tablecodec.EncodeTablePrefix(oldTableID),
				NewKeyPrefix: tablecodec.EncodeTablePrefix(tableID),
			},
		},
	}

	// range:     b   c d   e       g         i
	//            +---+ +---+       +---------+
	//          +-------------+----------+---------+
	// region:  a             f          h         j
	splitHelper := split.NewSplitHelper()
	splitHelper.Merge(split.Valued{Key: split.Span{StartKey: keyWithTablePrefix(oldTableID, "b"), EndKey: keyWithTablePrefix(oldTableID, "c")}, Value: split.Value{Size: 100, Number: 100}})
	splitHelper.Merge(split.Valued{Key: split.Span{StartKey: keyWithTablePrefix(oldTableID, "d"), EndKey: keyWithTablePrefix(oldTableID, "e")}, Value: split.Value{Size: 200, Number: 200}})
	splitHelper.Merge(split.Valued{Key: split.Span{StartKey: keyWithTablePrefix(oldTableID, "g"), EndKey: keyWithTablePrefix(oldTableID, "i")}, Value: split.Value{Size: 300, Number: 300}})
	client := newFakeSplitClient()
	client.AppendRegion(keyWithTablePrefix(tableID, "a"), keyWithTablePrefix(tableID, "f"))
	client.AppendRegion(keyWithTablePrefix(tableID, "f"), keyWithTablePrefix(tableID, "h"))
	client.AppendRegion(keyWithTablePrefix(tableID, "h"), keyWithTablePrefix(tableID, "j"))
	client.AppendRegion(keyWithTablePrefix(tableID, "j"), keyWithTablePrefix(tableID+1, "a"))

	iter := NewSplitHelperIteratorForTest(splitHelper, tableID, rewriteRules)
	err := SplitPoint(ctx, iter, client, func(ctx context.Context, rs *RegionSplitter, u uint64, o int64, ri *split.RegionInfo, v []split.Valued) error {
		require.Equal(t, u, uint64(0))
		require.Equal(t, o, int64(0))
		require.Equal(t, ri.Region.StartKey, keyWithTablePrefix(tableID, "a"))
		require.Equal(t, ri.Region.EndKey, keyWithTablePrefix(tableID, "f"))
		require.EqualValues(t, v[0].Key.StartKey, keyWithTablePrefix(tableID, "b"))
		require.EqualValues(t, v[0].Key.EndKey, keyWithTablePrefix(tableID, "c"))
		require.EqualValues(t, v[1].Key.StartKey, keyWithTablePrefix(tableID, "d"))
		require.EqualValues(t, v[1].Key.EndKey, keyWithTablePrefix(tableID, "e"))
		require.Equal(t, len(v), 2)
		return nil
	})
	require.NoError(t, err)
}

func getCharFromNumber(prefix string, i int) string {
	c := '1' + (i % 10)
	b := '1' + (i%100)/10
	a := '1' + i/100
	return fmt.Sprintf("%s%c%c%c", prefix, a, b, c)
}

func TestSplitPoint2(t *testing.T) {
	ctx := context.Background()
	var oldTableID int64 = 50
	var tableID int64 = 100
	rewriteRules := &RewriteRules{
		Data: []*import_sstpb.RewriteRule{
			{
				OldKeyPrefix: tablecodec.EncodeTablePrefix(oldTableID),
				NewKeyPrefix: tablecodec.EncodeTablePrefix(tableID),
			},
		},
	}

	// range:     b   c d   e f                 i j    k l        n
	//            +---+ +---+ +-----------------+ +----+ +--------+
	//          +---------------+--+.....+----+------------+---------+
	// region:  a               g   >128      h            m         o
	splitHelper := split.NewSplitHelper()
	splitHelper.Merge(split.Valued{Key: split.Span{StartKey: keyWithTablePrefix(oldTableID, "b"), EndKey: keyWithTablePrefix(oldTableID, "c")}, Value: split.Value{Size: 100, Number: 100}})
	splitHelper.Merge(split.Valued{Key: split.Span{StartKey: keyWithTablePrefix(oldTableID, "d"), EndKey: keyWithTablePrefix(oldTableID, "e")}, Value: split.Value{Size: 200, Number: 200}})
	splitHelper.Merge(split.Valued{Key: split.Span{StartKey: keyWithTablePrefix(oldTableID, "f"), EndKey: keyWithTablePrefix(oldTableID, "i")}, Value: split.Value{Size: 300, Number: 300}})
	splitHelper.Merge(split.Valued{Key: split.Span{StartKey: keyWithTablePrefix(oldTableID, "j"), EndKey: keyWithTablePrefix(oldTableID, "k")}, Value: split.Value{Size: 200, Number: 200}})
	splitHelper.Merge(split.Valued{Key: split.Span{StartKey: keyWithTablePrefix(oldTableID, "l"), EndKey: keyWithTablePrefix(oldTableID, "n")}, Value: split.Value{Size: 200, Number: 200}})
	client := newFakeSplitClient()
	client.AppendRegion(keyWithTablePrefix(tableID, "a"), keyWithTablePrefix(tableID, "g"))
	client.AppendRegion(keyWithTablePrefix(tableID, "g"), keyWithTablePrefix(tableID, getCharFromNumber("g", 0)))
	for i := 0; i < 256; i++ {
		client.AppendRegion(keyWithTablePrefix(tableID, getCharFromNumber("g", i)), keyWithTablePrefix(tableID, getCharFromNumber("g", i+1)))
	}
	client.AppendRegion(keyWithTablePrefix(tableID, getCharFromNumber("g", 256)), keyWithTablePrefix(tableID, "h"))
	client.AppendRegion(keyWithTablePrefix(tableID, "h"), keyWithTablePrefix(tableID, "m"))
	client.AppendRegion(keyWithTablePrefix(tableID, "m"), keyWithTablePrefix(tableID, "o"))
	client.AppendRegion(keyWithTablePrefix(tableID, "o"), keyWithTablePrefix(tableID+1, "a"))

	firstSplit := true
	iter := NewSplitHelperIteratorForTest(splitHelper, tableID, rewriteRules)
	err := SplitPoint(ctx, iter, client, func(ctx context.Context, rs *RegionSplitter, u uint64, o int64, ri *split.RegionInfo, v []split.Valued) error {
		if firstSplit {
			require.Equal(t, u, uint64(0))
			require.Equal(t, o, int64(0))
			require.Equal(t, ri.Region.StartKey, keyWithTablePrefix(tableID, "a"))
			require.Equal(t, ri.Region.EndKey, keyWithTablePrefix(tableID, "g"))
			require.EqualValues(t, v[0].Key.StartKey, keyWithTablePrefix(tableID, "b"))
			require.EqualValues(t, v[0].Key.EndKey, keyWithTablePrefix(tableID, "c"))
			require.EqualValues(t, v[1].Key.StartKey, keyWithTablePrefix(tableID, "d"))
			require.EqualValues(t, v[1].Key.EndKey, keyWithTablePrefix(tableID, "e"))
			require.EqualValues(t, v[2].Key.StartKey, keyWithTablePrefix(tableID, "f"))
			require.EqualValues(t, v[2].Key.EndKey, keyWithTablePrefix(tableID, "g"))
			require.Equal(t, v[2].Value.Size, uint64(1))
			require.Equal(t, v[2].Value.Number, int64(1))
			require.Equal(t, len(v), 3)
			firstSplit = false
		} else {
			require.Equal(t, u, uint64(1))
			require.Equal(t, o, int64(1))
			require.Equal(t, ri.Region.StartKey, keyWithTablePrefix(tableID, "h"))
			require.Equal(t, ri.Region.EndKey, keyWithTablePrefix(tableID, "m"))
			require.EqualValues(t, v[0].Key.StartKey, keyWithTablePrefix(tableID, "j"))
			require.EqualValues(t, v[0].Key.EndKey, keyWithTablePrefix(tableID, "k"))
			require.EqualValues(t, v[1].Key.StartKey, keyWithTablePrefix(tableID, "l"))
			require.EqualValues(t, v[1].Key.EndKey, keyWithTablePrefix(tableID, "m"))
			require.Equal(t, v[1].Value.Size, uint64(100))
			require.Equal(t, v[1].Value.Number, int64(100))
			require.Equal(t, len(v), 2)
		}
		return nil
	})
	require.NoError(t, err)
}

type fakeSplitClient struct {
	split.SplitClient
	regions []*split.RegionInfo
}

func newFakeSplitClient() *fakeSplitClient {
	return &fakeSplitClient{
		regions: make([]*split.RegionInfo, 0),
	}
}

func (f *fakeSplitClient) AppendRegion(startKey, endKey []byte) {
	f.regions = append(f.regions, &split.RegionInfo{
		Region: &metapb.Region{
			StartKey: startKey,
			EndKey:   endKey,
		},
	})
}

func (f *fakeSplitClient) ScanRegions(ctx context.Context, startKey, endKey []byte, limit int) ([]*split.RegionInfo, error) {
	result := make([]*split.RegionInfo, 0)
	count := 0
	for _, rng := range f.regions {
		if bytes.Compare(rng.Region.StartKey, endKey) <= 0 && bytes.Compare(rng.Region.EndKey, startKey) > 0 {
			result = append(result, rng)
			count++
		}
		if count >= limit {
			break
		}
	}
	return result, nil
}

func TestGetRewriteTableID(t *testing.T) {
	var tableID int64 = 76
	var oldTableID int64 = 80
	{
		rewriteRules := &RewriteRules{
			Data: []*import_sstpb.RewriteRule{
				{
					OldKeyPrefix: tablecodec.EncodeTablePrefix(oldTableID),
					NewKeyPrefix: tablecodec.EncodeTablePrefix(tableID),
				},
			},
		}

		newTableID := GetRewriteTableID(oldTableID, rewriteRules)
		require.Equal(t, tableID, newTableID)
	}

	{
		rewriteRules := &RewriteRules{
			Data: []*import_sstpb.RewriteRule{
				{
					OldKeyPrefix: tablecodec.GenTableRecordPrefix(oldTableID),
					NewKeyPrefix: tablecodec.GenTableRecordPrefix(tableID),
				},
			},
		}

		newTableID := GetRewriteTableID(oldTableID, rewriteRules)
		require.Equal(t, tableID, newTableID)
	}
}

type mockLogIter struct {
	next int
}

func (m *mockLogIter) TryNext(ctx context.Context) iter.IterResult[*LogDataFileInfo] {
	if m.next > 10000 {
		return iter.Done[*LogDataFileInfo]()
	}
	m.next += 1
	return iter.Emit(&LogDataFileInfo{
		DataFileInfo: &backuppb.DataFileInfo{
			StartKey: []byte(fmt.Sprintf("a%d", m.next)),
			EndKey:   []byte("b"),
			Length:   1024, // 1 KB
		},
	})
}

func TestLogFilesIterWithSplitHelper(t *testing.T) {
	var tableID int64 = 76
	var oldTableID int64 = 80
	rewriteRules := &RewriteRules{
		Data: []*import_sstpb.RewriteRule{
			{
				OldKeyPrefix: tablecodec.EncodeTablePrefix(oldTableID),
				NewKeyPrefix: tablecodec.EncodeTablePrefix(tableID),
			},
		},
	}
	rewriteRulesMap := map[int64]*RewriteRules{
		oldTableID: rewriteRules,
	}
	mockIter := &mockLogIter{}
	ctx := context.Background()
	logIter := NewLogFilesIterWithSplitHelper(mockIter, rewriteRulesMap, newFakeSplitClient(), 144*1024*1024, 1440000)
	next := 0
	for r := logIter.TryNext(ctx); !r.Finished; r = logIter.TryNext(ctx) {
		require.NoError(t, r.Err)
		next += 1
		require.Equal(t, []byte(fmt.Sprintf("a%d", next)), r.Item.StartKey)
	}
}

func regionInfo(startKey, endKey string) *split.RegionInfo {
	return &split.RegionInfo{
		Region: &metapb.Region{
			StartKey: []byte(startKey),
			EndKey:   []byte(endKey),
		},
	}
}

func TestSplitCheckPartRegionConsistency(t *testing.T) {
	var (
		startKey []byte = []byte("a")
		endKey   []byte = []byte("f")
		err      error
	)
	err = split.CheckPartRegionConsistency(startKey, endKey, nil)
	require.Error(t, err)
	err = split.CheckPartRegionConsistency(startKey, endKey, []*split.RegionInfo{
		regionInfo("b", "c"),
	})
	require.Error(t, err)
	err = split.CheckPartRegionConsistency(startKey, endKey, []*split.RegionInfo{
		regionInfo("a", "c"),
		regionInfo("d", "e"),
	})
	require.Error(t, err)
	err = split.CheckPartRegionConsistency(startKey, endKey, []*split.RegionInfo{
		regionInfo("a", "c"),
		regionInfo("c", "d"),
	})
	require.NoError(t, err)
	err = split.CheckPartRegionConsistency(startKey, endKey, []*split.RegionInfo{
		regionInfo("a", "c"),
		regionInfo("c", "d"),
		regionInfo("d", "f"),
	})
	require.NoError(t, err)
	err = split.CheckPartRegionConsistency(startKey, endKey, []*split.RegionInfo{
		regionInfo("a", "c"),
		regionInfo("c", "z"),
	})
	require.NoError(t, err)
}

func TestGetSplitSortedKeysFromSortedRegions(t *testing.T) {
	splitContext := SplitContext{}
	sortedKeys := [][]byte{
		[]byte("b"),
		[]byte("d"),
		[]byte("g"),
		[]byte("j"),
		[]byte("l"),
	}
	sortedRegions := []*split.RegionInfo{
		{
			Region: &metapb.Region{
				Id:       1,
				StartKey: []byte("a"),
				EndKey:   []byte("g"),
			},
		},
		{
			Region: &metapb.Region{
				Id:       2,
				StartKey: []byte("g"),
				EndKey:   []byte("k"),
			},
		},
		{
			Region: &metapb.Region{
				Id:       3,
				StartKey: []byte("k"),
				EndKey:   []byte("m"),
			},
		},
	}
	result := TestGetSplitSortedKeysFromSortedRegionsTest(splitContext, sortedKeys, sortedRegions)
	require.Equal(t, 3, len(result))
	require.Equal(t, [][]byte{[]byte("b"), []byte("d")}, result[1])
	require.Equal(t, [][]byte{[]byte("g"), []byte("j")}, result[2])
	require.Equal(t, [][]byte{[]byte("l")}, result[3])
}
