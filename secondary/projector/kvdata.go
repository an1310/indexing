// data-path concurrency model:
//
//               back-channel
//     feed <---------------------*   NewKVData()
//                StreamRequest   |     |            *---> vbucket
//                    StreamEnd   |   (spawn)        |
//                                |     |            *---> vbucket
//                                |     |            |
//        AddEngines() --*-----> runScatter ---------*---> vbucket
//                       |
//     DeleteEngines() --*
//                       |
//     GetStatistics() --*
//                       |
//             Close() --*

package projector

import "fmt"
import "strconv"
import "runtime/debug"

import mcd "github.com/couchbase/gomemcached"
import mc "github.com/couchbase/gomemcached/client"
import c "github.com/couchbase/indexing/secondary/common"
import "github.com/couchbase/indexing/secondary/protobuf"

// KVData captures an instance of data-path for single kv-node
// from upstream connection.
type KVData struct {
	feed   *Feed
	topic  string // immutable
	bucket string // immutable
	kvaddr string // immutable
	vrs    map[uint16]*VbucketRoutine
	// evaluators and subscribers
	engines   map[uint64]*Engine
	endpoints map[string]c.RouterEndpoint
	// server channels
	sbch  chan []interface{}
	finch chan bool
	// misc.
	logPrefix string
}

// NewKVData create a new data-path instance.
func NewKVData(
	feed *Feed, bucket, kvaddr string,
	reqTs *protobuf.TsVbuuid,
	engines map[uint64]*Engine,
	endpoints map[string]c.RouterEndpoint,
	mutch <-chan *mc.UprEvent) *KVData {

	kvdata := &KVData{
		feed:      feed,
		topic:     feed.topic,
		bucket:    bucket,
		kvaddr:    kvaddr,
		vrs:       make(map[uint16]*VbucketRoutine),
		engines:   make(map[uint64]*Engine),
		endpoints: make(map[string]c.RouterEndpoint),
		// 16 is enough, there can't be more than that many out-standing
		// control calls on this feed.
		sbch:      make(chan []interface{}, 16),
		finch:     make(chan bool),
		logPrefix: fmt.Sprintf("[%v->%v->%v]", feed.topic, bucket, kvaddr),
	}
	for uuid, engine := range engines {
		kvdata.engines[uuid] = engine
	}
	for raddr, endpoint := range endpoints {
		kvdata.endpoints[raddr] = endpoint
	}
	go kvdata.runScatter(reqTs, mutch)
	c.Infof("%v started ...\n", kvdata.logPrefix)
	return kvdata
}

// commands to server
const (
	kvCmdAddEngines byte = iota + 1
	kvCmdDelEngines
	kvCmdTs
	kvCmdGetStats
	kvCmdClose
)

// AddEngines and endpoints, synchronous call.
func (kvdata *KVData) AddEngines(
	engines map[uint64]*Engine, endpoints map[string]c.RouterEndpoint) error {

	respch := make(chan []interface{}, 1)
	cmd := []interface{}{kvCmdAddEngines, engines, endpoints, respch}
	_, err := c.FailsafeOp(kvdata.sbch, respch, cmd, kvdata.finch)
	return err
}

// DeleteEngines synchronous call.
func (kvdata *KVData) DeleteEngines(engineKeys []uint64) error {
	respch := make(chan []interface{}, 1)
	cmd := []interface{}{kvCmdDelEngines, engineKeys, respch}
	_, err := c.FailsafeOp(kvdata.sbch, respch, cmd, kvdata.finch)
	return err
}

// UpdateTs with new set of {vbno,seqno}, synchronous call.
func (kvdata *KVData) UpdateTs(ts *protobuf.TsVbuuid) error {
	respch := make(chan []interface{}, 1)
	cmd := []interface{}{kvCmdTs, ts}
	_, err := c.FailsafeOp(kvdata.sbch, respch, cmd, kvdata.finch)
	return err
}

// GetStatistics from kv data path, synchronous call.
func (kvdata *KVData) GetStatistics() map[string]interface{} {
	respch := make(chan []interface{}, 1)
	cmd := []interface{}{kvCmdGetStats, respch}
	resp, _ := c.FailsafeOp(kvdata.sbch, respch, cmd, kvdata.finch)
	return resp[0].(map[string]interface{})
}

// Close kvdata kv data path, synchronous call.
func (kvdata *KVData) Close() error {
	respch := make(chan []interface{}, 1)
	cmd := []interface{}{kvCmdClose, respch}
	_, err := c.FailsafeOp(kvdata.sbch, respch, cmd, kvdata.finch)
	return err
}

// go-routine handles data path.
func (kvdata *KVData) runScatter(
	ts *protobuf.TsVbuuid, mutch <-chan *mc.UprEvent) {

	stats := kvdata.newStats()

	defer func() {
		if r := recover(); r != nil {
			c.Errorf("%v runScatter() crashed: %v\n", kvdata.logPrefix, r)
			c.StackTrace(string(debug.Stack()))
		}
		kvdata.publishStreamEnd()
		kvdata.feed.PostFinKVdata(kvdata.bucket, kvdata.kvaddr)
		close(kvdata.finch)
		c.Infof("%v for %q ... stopped\n", kvdata.logPrefix, kvdata.kvaddr)
	}()

	eventCount := 0

loop:
	for {
		select {
		case m, ok := <-mutch:
			if ok == false { // upstream has closed
				break loop
			}
			kvdata.scatterMutation(m, ts)
			eventCount++

			// all vbuckets have ended for this stream, exit kvdata.
			if len(kvdata.vrs) == 0 {
				break loop
			}

		case msg := <-kvdata.sbch:
			cmd := msg[0].(byte)
			switch cmd {
			case kvCmdAddEngines:
				respch := msg[3].(chan []interface{})
				if msg[1] != nil {
					for uuid, engine := range msg[1].(map[uint64]*Engine) {
						kvdata.engines[uuid] = engine
					}
				}
				if msg[2] != nil {
					rv := msg[2].(map[string]c.RouterEndpoint)
					for raddr, endp := range rv {
						kvdata.endpoints[raddr] = endp
					}
				}
				if kvdata.engines != nil || kvdata.endpoints != nil {
					for _, vr := range kvdata.vrs {
						vr.AddEngines(kvdata.engines, kvdata.endpoints)
					}
				}
				respch <- []interface{}{nil}

			case kvCmdDelEngines:
				engineKeys := msg[1].([]uint64)
				respch := msg[2].(chan []interface{})
				for _, vr := range kvdata.vrs {
					vr.DeleteEngines(engineKeys)
				}
				for _, engineKey := range engineKeys {
					delete(kvdata.engines, engineKey)
				}
				respch <- []interface{}{nil}

			case kvCmdTs:
				ts = ts.Union(msg[1].(*protobuf.TsVbuuid))
				respch := msg[2].(chan []interface{})
				respch <- []interface{}{nil}

			case kvCmdGetStats:
				respch := msg[1].(chan []interface{})
				statVbuckets := make(map[string]interface{})
				for i, vr := range kvdata.vrs {
					statVbuckets[strconv.Itoa(int(i))] = vr.GetStatistics()
				}
				stats.Set("vbuckets", statVbuckets)
				respch <- []interface{}{map[string]interface{}(stats)}

			case kvCmdClose:
				respch := msg[1].(chan []interface{})
				respch <- []interface{}{nil}
				break loop
			}
		}
	}
}

func (kvdata *KVData) scatterMutation(
	m *mc.UprEvent, ts *protobuf.TsVbuuid) (err error) {

	vbno := m.VBucket

	switch m.Opcode {
	case mcd.UPR_STREAMREQ:
		if m.Status == mcd.ROLLBACK {
			c.Infof("%v StreamRequest ROLLBACK: %v\n", kvdata.logPrefix, m)

		} else if m.Status != mcd.SUCCESS {
			msg := "%v StreamRequest Status: %s, %v\n"
			c.Errorf(msg, kvdata.logPrefix, m.Status, m)

		} else if _, ok := kvdata.vrs[vbno]; ok {
			msg := "%v duplicate OpStreamRequest for %v\n"
			c.Errorf(msg, kvdata.logPrefix, vbno)

		} else if m.VBuuid, _, err = m.FailoverLog.Latest(); err != nil {
			panic(err)

		} else {
			c.Debugf("%v StreamRequest %v\n", kvdata.logPrefix, m)
			topic, bucket, kv := kvdata.topic, kvdata.bucket, kvdata.kvaddr
			m.Seqno, _ = ts.SeqnoFor(vbno)
			config := kvdata.feed.config
			vr := NewVbucketRoutine(
				topic, bucket, kv, vbno, m.VBuuid, m.Seqno, config)
			vr.AddEngines(kvdata.engines, kvdata.endpoints)
			vr.Event(m)
			kvdata.vrs[vbno] = vr
		}
		kvdata.feed.PostStreamRequest(kvdata.bucket, kvdata.kvaddr, m)

	case mcd.UPR_STREAMEND:
		if vr, ok := kvdata.vrs[vbno]; !ok {
			c.Errorf("%v duplicate OpStreamEnd for %v\n", kvdata.logPrefix, vbno)

		} else if m.Status != mcd.SUCCESS {
			msg := "%v StreamEnd Status: %s, %v\n"
			c.Errorf(msg, kvdata.logPrefix, m.Status, m)

		} else {
			c.Debugf("%v StreamEnd %v\n", kvdata.logPrefix, m)
			vr.Event(m)
			delete(kvdata.vrs, vbno)
		}
		kvdata.feed.PostStreamEnd(kvdata.bucket, kvdata.kvaddr, m)

	case mcd.UPR_MUTATION, mcd.UPR_DELETION, mcd.UPR_SNAPSHOT, mcd.UPR_EXPIRATION:
		if vr, ok := kvdata.vrs[vbno]; ok {
			vr.Event(m)

		} else {
			c.Errorf("%v unknown vbucket %v\n", kvdata.logPrefix, vbno)
		}
	}
	return
}

func (kvdata *KVData) publishStreamEnd() {
	m := &mc.UprEvent{Opcode: mcd.UPR_STREAMEND, Status: mcd.SUCCESS}
	for _, vr := range kvdata.vrs {
		vr.Event(m)
	}
}

func (kvdata *KVData) newStats() c.Statistics {
	statVbuckets := make(map[string]interface{})
	m := map[string]interface{}{
		"events":   float64(0),   // no. of mutations events received
		"vbuckets": statVbuckets, // per vbucket statistics
	}
	stats, _ := c.NewStatistics(m)
	return stats
}