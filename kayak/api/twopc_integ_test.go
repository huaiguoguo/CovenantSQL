/*
 * Copyright 2018 The ThunderDB Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package api

import (
	"context"
	"crypto/rand"
	"errors"
	"io/ioutil"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	. "github.com/smartystreets/goconvey/convey"
	"github.com/stretchr/testify/mock"
	"gitlab.com/thunderdb/ThunderDB/conf"
	"gitlab.com/thunderdb/ThunderDB/crypto/asymmetric"
	"gitlab.com/thunderdb/ThunderDB/crypto/etls"
	"gitlab.com/thunderdb/ThunderDB/crypto/hash"
	"gitlab.com/thunderdb/ThunderDB/kayak"
	kt "gitlab.com/thunderdb/ThunderDB/kayak/transport"
	"gitlab.com/thunderdb/ThunderDB/proto"
	"gitlab.com/thunderdb/ThunderDB/rpc"
	"gitlab.com/thunderdb/ThunderDB/twopc"
	"gitlab.com/thunderdb/ThunderDB/utils/log"
)

var (
	pass = "DU>p~[/dd2iImUs*"
)

// MockWorker is an autogenerated mock type for the Worker type
type MockWorker struct {
	mock.Mock
}

// Commit provides a mock function with given fields: ctx, wb
func (_m *MockWorker) Commit(ctx context.Context, wb twopc.WriteBatch) error {
	ret := _m.Called(ctx, wb)

	var r0 error
	if rf, ok := ret.Get(0).(func(context.Context, twopc.WriteBatch) error); ok {
		r0 = rf(ctx, wb)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// Prepare provides a mock function with given fields: ctx, wb
func (_m *MockWorker) Prepare(ctx context.Context, wb twopc.WriteBatch) error {
	ret := _m.Called(ctx, wb)

	var r0 error
	if rf, ok := ret.Get(0).(func(context.Context, twopc.WriteBatch) error); ok {
		r0 = rf(ctx, wb)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

// Rollback provides a mock function with given fields: ctx, wb
func (_m *MockWorker) Rollback(ctx context.Context, wb twopc.WriteBatch) error {
	ret := _m.Called(ctx, wb)

	var r0 error
	if rf, ok := ret.Get(0).(func(context.Context, twopc.WriteBatch) error); ok {
		r0 = rf(ctx, wb)
	} else {
		r0 = ret.Error(0)
	}

	return r0
}

type CallCollector struct {
	l         sync.Mutex
	callOrder []string
}

func (c *CallCollector) Append(call string) {
	c.l.Lock()
	defer c.l.Unlock()
	c.callOrder = append(c.callOrder, call)
}

func (c *CallCollector) Get() []string {
	c.l.Lock()
	defer c.l.Unlock()
	return c.callOrder[:]
}

func (c *CallCollector) Reset() {
	c.l.Lock()
	defer c.l.Unlock()
	c.callOrder = c.callOrder[:0]
}

type mockRes struct {
	rootDir    string
	nodeID     proto.NodeID
	worker     *MockWorker
	server     *rpc.Server
	config     kayak.Config
	runtime    *kayak.Runtime
	listenAddr string
}

func cipherHandler(conn net.Conn) (cryptoConn *etls.CryptoConn, err error) {
	nodeIDBuf := make([]byte, hash.HashBSize)
	rCount, err := conn.Read(nodeIDBuf)
	if err != nil || rCount != hash.HashBSize {
		return
	}
	cipher := etls.NewCipher([]byte(pass))
	h, _ := hash.NewHash(nodeIDBuf)
	nodeID := &proto.RawNodeID{
		Hash: *h,
	}

	cryptoConn = etls.NewConn(conn, cipher, nodeID)
	return
}

func getNodeDialer(reqNodeID proto.NodeID, nodeMap *sync.Map) kt.ETLSRPCClientBuilder {
	return func(ctx context.Context, nodeID proto.NodeID) (client *rpc.Client, err error) {
		cipher := etls.NewCipher([]byte(pass))

		var ok bool
		var addr interface{}

		if addr, ok = nodeMap.Load(nodeID); !ok {
			return nil, errors.New("could not connect to " + string(nodeID))
		}

		var conn net.Conn
		conn, err = net.Dial("tcp", addr.(string))
		if err != nil {
			return
		}

		// convert node id to raw node id
		h, _ := hash.NewHashFromStr(string(reqNodeID))
		rawNodeID := &proto.RawNodeID{Hash: *h}
		wCount, err := conn.Write(rawNodeID.Hash[:])
		if err != nil || wCount != hash.HashBSize {
			return
		}

		cryptConn := etls.NewConn(conn, cipher, rawNodeID)
		return rpc.InitClientConn(cryptConn)
	}
}

func testWithNewNode(nodeMap *sync.Map) (mock *mockRes, err error) {
	mock = &mockRes{}

	// mock etls without kms server
	addr := "127.0.0.1:0"
	var l *etls.CryptoListener
	l, err = etls.NewCryptoListener("tcp", addr, cipherHandler)
	if err != nil {
		return
	}
	mock.listenAddr = l.Addr().String()

	// random node id
	randBytes := make([]byte, 4)
	rand.Read(randBytes)
	mock.nodeID = proto.NodeID(hash.THashH(randBytes).String())
	nodeMap.Store(mock.nodeID, mock.listenAddr)

	// mock rpc server
	mock.server, err = rpc.NewServerWithService(rpc.ServiceMap{})
	if err != nil {
		return
	}
	mock.server.SetListener(l)

	// create mux service for kayak
	service := NewMuxService("Kayak", mock.server)
	mock.rootDir, err = ioutil.TempDir("", "kayak_test")
	if err != nil {
		return
	}

	// worker
	mock.worker = &MockWorker{}

	// create two pc config
	options := NewTwoPCOptions().
		WithNodeID(mock.nodeID).
		WithClientBuilder(getNodeDialer(mock.nodeID, nodeMap)).
		WithProcessTimeout(time.Millisecond * 300).
		WithTransportID(DefaultTransportID).
		WithLogger(log.StandardLogger())
	mock.config = NewTwoPCConfigWithOptions(mock.rootDir, service, mock.worker, options)

	return
}

func createRuntime(peers *kayak.Peers, mock *mockRes) (err error) {
	mock.runtime, err = NewTwoPCKayak(peers, mock.config)
	return
}

func testPeersFixture(term uint64, servers []*kayak.Server) *kayak.Peers {
	testPriv := []byte{
		0xea, 0xf0, 0x2c, 0xa3, 0x48, 0xc5, 0x24, 0xe6,
		0x39, 0x26, 0x55, 0xba, 0x4d, 0x29, 0x60, 0x3c,
		0xd1, 0xa7, 0x34, 0x7d, 0x9d, 0x65, 0xcf, 0xe9,
		0x3c, 0xe1, 0xeb, 0xff, 0xdc, 0xa2, 0x26, 0x94,
	}
	privKey, pubKey := asymmetric.PrivKeyFromBytes(testPriv)

	newServers := make([]*kayak.Server, 0, len(servers))
	var leaderNode *kayak.Server

	for _, s := range servers {
		newS := &kayak.Server{
			Role:   s.Role,
			ID:     s.ID,
			PubKey: pubKey,
		}
		newServers = append(newServers, newS)
		if newS.Role == conf.Leader {
			leaderNode = newS
		}
	}

	peers := &kayak.Peers{
		Term:    term,
		Leader:  leaderNode,
		Servers: servers,
		PubKey:  pubKey,
	}

	peers.Sign(privKey)

	return peers
}

func TestExampleTwoPCCommit(t *testing.T) {
	// cleanup log storage after execution
	cleanupDir := func(c *mockRes) {
		os.RemoveAll(c.rootDir)
	}

	// only commit logic
	Convey("commit", t, func() {
		var err error

		var nodeMap sync.Map

		lMock, err := testWithNewNode(&nodeMap)
		So(err, ShouldBeNil)
		f1Mock, err := testWithNewNode(&nodeMap)
		So(err, ShouldBeNil)
		f2Mock, err := testWithNewNode(&nodeMap)
		So(err, ShouldBeNil)

		// peers is a simple 3-node peer configuration
		peers := testPeersFixture(1, []*kayak.Server{
			{
				Role: conf.Leader,
				ID:   lMock.nodeID,
			},
			{
				Role: conf.Follower,
				ID:   f1Mock.nodeID,
			},
			{
				Role: conf.Follower,
				ID:   f2Mock.nodeID,
			},
		})
		defer cleanupDir(lMock)
		defer cleanupDir(f1Mock)
		defer cleanupDir(f2Mock)

		// create runtime
		err = createRuntime(peers, lMock)
		So(err, ShouldBeNil)
		err = createRuntime(peers, f1Mock)
		So(err, ShouldBeNil)
		err = createRuntime(peers, f2Mock)
		So(err, ShouldBeNil)

		// init
		err = lMock.runtime.Init()
		So(err, ShouldBeNil)
		err = f1Mock.runtime.Init()
		So(err, ShouldBeNil)
		err = f2Mock.runtime.Init()
		So(err, ShouldBeNil)

		// payload to send
		testPayload := []byte("test data")

		// underlying worker mock, prepare/commit/rollback with be received the decoded data
		callOrder := &CallCollector{}
		f1Mock.worker.On("Prepare", mock.Anything, testPayload).
			Return(nil).Run(func(args mock.Arguments) {
			callOrder.Append("f_prepare")
		})
		f2Mock.worker.On("Prepare", mock.Anything, testPayload).
			Return(nil).Run(func(args mock.Arguments) {
			callOrder.Append("f_prepare")
		})
		f1Mock.worker.On("Commit", mock.Anything, testPayload).
			Return(nil).Run(func(args mock.Arguments) {
			callOrder.Append("f_commit")
		})
		f2Mock.worker.On("Commit", mock.Anything, testPayload).
			Return(nil).Run(func(args mock.Arguments) {
			callOrder.Append("f_commit")
		})
		lMock.worker.On("Prepare", mock.Anything, testPayload).
			Return(nil).Run(func(args mock.Arguments) {
			callOrder.Append("l_prepare")
		})
		lMock.worker.On("Commit", mock.Anything, testPayload).
			Return(nil).Run(func(args mock.Arguments) {
			callOrder.Append("l_commit")
		})

		// start server
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			lMock.server.Serve()
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			f1Mock.server.Serve()
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			f2Mock.server.Serve()
		}()

		// process the encoded data
		err = lMock.runtime.Apply(testPayload)
		So(err, ShouldBeNil)
		So(callOrder.Get(), ShouldResemble, []string{
			"f_prepare",
			"f_prepare",
			"l_prepare",
			"f_commit",
			"f_commit",
			"l_commit",
		})

		// process the encoded data again
		callOrder.Reset()
		err = lMock.runtime.Apply(testPayload)
		So(err, ShouldBeNil)
		So(callOrder.Get(), ShouldResemble, []string{
			"f_prepare",
			"f_prepare",
			"l_prepare",
			"f_commit",
			"f_commit",
			"l_commit",
		})

		// shutdown
		lMock.runtime.Shutdown()
		f1Mock.runtime.Shutdown()
		f2Mock.runtime.Shutdown()

		// close
		lMock.server.Listener.Close()
		f1Mock.server.Listener.Close()
		f2Mock.server.Listener.Close()
		lMock.server.Stop()
		f1Mock.server.Stop()
		f2Mock.server.Stop()

		wg.Wait()
	})
}
