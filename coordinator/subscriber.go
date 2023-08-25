/*
Copyright 2023 Huawei Cloud Computing Technologies Co., Ltd.

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

package coordinator

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/apache/arrow/go/arrow/array"
	"github.com/apache/arrow/go/arrow/flight"
	"github.com/apache/arrow/go/arrow/ipc"
	"github.com/openGemini/openGemini/lib/config"
	"github.com/openGemini/openGemini/lib/logger"
	"github.com/openGemini/openGemini/lib/util"
	"github.com/openGemini/openGemini/open_src/influx/meta"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type Client interface {
	Send(db, rp string, lineProtocol []byte) error
	SendColumn(db, rp, mst string, record array.Record) error
	Destination() string
}

type HTTPClient struct {
	client *http.Client
	url    *url.URL
}

func (c *HTTPClient) Send(db, rp string, lineProtocol []byte) error {
	r := bytes.NewReader(lineProtocol)
	req, err := http.NewRequest("POST", c.url.String()+"/write", r)
	if err != nil {
		return err
	}

	params := req.URL.Query()
	params.Set("db", db)
	params.Set("rp", rp)
	req.URL.RawQuery = params.Encode()

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return err
		}
		err = fmt.Errorf(string(body))
		return err
	}
	return nil
}

func (c *HTTPClient) SendColumn(db, rp, mst string, record array.Record) error {
	return errors.New("http client dosen't send column")
}

func (c *HTTPClient) Destination() string {
	return c.url.String()
}

func NewHTTPClient(url *url.URL, timeout time.Duration) *HTTPClient {
	c := &http.Client{Timeout: timeout}
	return &HTTPClient{client: c, url: url}
}

func NewHTTPSClient(url *url.URL, timeout time.Duration, skipVerify bool, certs string) (*HTTPClient, error) {
	tlsConfig := &tls.Config{InsecureSkipVerify: skipVerify}
	if certs != "" {
		caCert, err := os.ReadFile(certs)
		if err != nil {
			return nil, err
		}
		tlsConfig.RootCAs = x509.NewCertPool()
		tlsConfig.RootCAs.AppendCertsFromPEM(caCert)
	}

	transport := &http.Transport{
		TLSClientConfig: tlsConfig,
	}
	c := &http.Client{Timeout: timeout, Transport: transport}
	return &HTTPClient{client: c, url: url}, nil
}

type RPCClient struct {
	// todo
	address string
	client  flight.FlightService_DoPutClient
}

func (c *RPCClient) Send(db, rp string, lineProtocol []byte) error {
	return errors.New("rpc client dosen't send line protocol")
}

func (c *RPCClient) SendColumn(db, rp, mst string, record array.Record) error {
	wr := flight.NewRecordWriter(c.client, ipc.WithSchema(record.Schema()))
	// err未处理
	defer wr.Close()
	path := fmt.Sprintf("{\"db\": \"%s\", \"rp\": \"%s\", \"mst\": \"%s\"}", db, rp, mst)
	wr.SetFlightDescriptor(&flight.FlightDescriptor{Path: []string{path}})
	err := wr.Write(record)
	if err != nil {
		return err
	}
	return nil
}

func (c *RPCClient) Destination() string {
	return c.address
}

// clientAuth这块需要改
var Token = "token"

type clientAuth struct {
	token string
}

func (a *clientAuth) Authenticate(ctx context.Context, c flight.AuthConn) error {
	if err := c.Send(ctx.Value(Token).([]byte)); err != nil {
		return err
	}

	token, err := c.Read()
	a.token = util.Bytes2str(token)
	return err
}

func (a *clientAuth) GetToken(_ context.Context) (string, error) {
	return a.token, nil
}

func NewRPCClient(address string) (*RPCClient, error) {
	// todo 错误检查, 后续这些client需要关闭
	authClient := &clientAuth{}
	client, _ := flight.NewFlightClient(address, authClient, grpc.WithTransportCredentials(insecure.NewCredentials()))
	doPutClient, _ := client.DoPut(context.Background())
	return &RPCClient{address: address, client: doPutClient}, nil
}

type WriteRequest struct {
	Client int

	// for line protocol
	LineProtocol []byte

	// for column protocol
	Mst    string
	Record array.Record
}

type BaseWriter struct {
	ch      chan WriteRequest
	clients []Client
	db      string
	rp      string
	name    string
	logger  *logger.Logger
}

func NewBaseWriter(db, rp, name string, clients []Client, logger *logger.Logger) BaseWriter {
	return BaseWriter{db: db, rp: rp, name: name, clients: clients, logger: logger}
}

func (w *BaseWriter) Send(wr WriteRequest) {
	select {
	case w.ch <- wr:
	default:
		w.logger.Error("failed to send write request to write buffer", zap.String("dest", w.clients[wr.Client].Destination()),
			zap.String("db", w.db), zap.String("rp", w.rp))
	}
}

func (w *BaseWriter) Run() {
	for wr := range w.ch {
		var err error
		if wr.LineProtocol != nil {
			err = w.clients[wr.Client].Send(w.db, w.rp, wr.LineProtocol)
		} else {
			err = w.clients[wr.Client].SendColumn(w.db, w.rp, wr.Mst, wr.Record)
		}
		if err != nil {
			w.logger.Error("failed to forward write request", zap.String("dest", w.clients[wr.Client].Destination()),
				zap.String("db", w.db), zap.String("rp", w.rp), zap.Error(err))
		}
	}
}

func (w *BaseWriter) Name() string {
	return w.name
}

func (w *BaseWriter) Clients() []Client {
	return w.clients
}

func (w *BaseWriter) Start(concurrency, buffersize int) {
	w.ch = make(chan WriteRequest, buffersize)
	for i := 0; i < concurrency; i++ {
		go w.Run()
	}
}

func (w *BaseWriter) Stop() {
	close(w.ch)
}

type SubscriberWriter interface {
	Write(lineProtocol []byte)
	WriteColumn(mst string, record array.Record)
	Name() string
	Run()
	Start(concurrency, buffersize int)
	Stop()
	Clients() []Client
}

type AllWriter struct {
	BaseWriter
}

func (w *AllWriter) Write(lineProtocol []byte) {
	for i := 0; i < len(w.clients); i++ {
		wr := WriteRequest{Client: i, LineProtocol: lineProtocol}
		w.Send(wr)
	}
}

func (w *AllWriter) WriteColumn(mst string, record array.Record) {
	for i := 0; i < len(w.clients); i++ {
		wr := WriteRequest{Client: i, Mst: mst, Record: record}
		w.Send(wr)
	}
}

type RoundRobinWriter struct {
	BaseWriter
	i    int
	lock sync.Mutex
}

func (w *RoundRobinWriter) Write(lineProtocol []byte) {
	w.lock.Lock()
	i := w.i
	w.i = (w.i + 1) % len(w.clients)
	w.lock.Unlock()
	wr := WriteRequest{Client: i, LineProtocol: lineProtocol}
	w.Send(wr)
}

func (w *RoundRobinWriter) WriteColumn(mst string, record array.Record) {
	w.lock.Lock()
	i := w.i
	w.i = (w.i + 1) % len(w.clients)
	w.lock.Unlock()
	wr := WriteRequest{Client: i, Mst: mst, Record: record}
	w.Send(wr)
}

type MetaClient interface {
	Databases() map[string]*meta.DatabaseInfo
	Database(string) (*meta.DatabaseInfo, error)
	GetMaxSubscriptionID() uint64
	WaitForDataChanged() chan struct{}
}

type SubscriberManager struct {
	lock           sync.RWMutex
	writers        map[string]map[string][]SubscriberWriter // {"db0": {"rp0": []SubscriberWriter }}
	client         MetaClient
	config         config.Subscriber
	Logger         *logger.Logger
	lastModifiedID uint64
}

func (s *SubscriberManager) NewSubscriberWriter(db, rp, name, mode string, destinations []string) (SubscriberWriter, error) {
	clients := make([]Client, 0, len(destinations))
	for _, dest := range destinations {
		u, err := url.Parse(dest)
		if err != nil {
			return nil, fmt.Errorf("fail to parse %s", err)
		}
		var c Client
		switch u.Scheme {
		case "http":
			c = NewHTTPClient(u, time.Duration(s.config.HTTPTimeout))
		case "https":
			c, err = NewHTTPSClient(u, time.Duration(s.config.HTTPTimeout), s.config.InsecureSkipVerify, s.config.HttpsCertificate)
			if err != nil {
				return nil, err
			}
		// todo: 加个校验，同一个订阅，要么全是http/https，要么全是rpc，否则报错
		case "rpc":
			c, err = NewRPCClient(u.Host)
			if err != nil {
				return nil, err
			}
		default:
			return nil, fmt.Errorf("unknown subscription schema %s", u.Scheme)
		}
		clients = append(clients, c)
	}
	switch mode {
	case "ALL":
		return &AllWriter{BaseWriter: NewBaseWriter(db, rp, name, clients, s.Logger)}, nil
	case "ANY":
		return &RoundRobinWriter{BaseWriter: NewBaseWriter(db, rp, name, clients, s.Logger)}, nil
	}
	return nil, fmt.Errorf("unknown subscription mode %s", mode)
}

func (s *SubscriberManager) InitWriters() {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.WalkDatabases(func(dbi *meta.DatabaseInfo) {
		s.writers[dbi.Name] = make(map[string][]SubscriberWriter)
		dbi.WalkRetentionPolicy(func(rpi *meta.RetentionPolicyInfo) {
			writers := make([]SubscriberWriter, 0, len(rpi.Subscriptions))
			for _, sub := range rpi.Subscriptions {
				writer, err := s.NewSubscriberWriter(dbi.Name, rpi.Name, sub.Name, sub.Mode, sub.Destinations)
				if err != nil {
					s.Logger.Error("fail to create subscriber", zap.String("db", dbi.Name), zap.String("rp", rpi.Name), zap.String("sub", sub.Name),
						zap.Strings("dest", sub.Destinations))
				} else {
					writers = append(writers, writer)
					writer.Start(s.config.WriteConcurrency, s.config.WriteBufferSize)
					s.Logger.Info("initialize subscriber writer", zap.String("db", dbi.Name), zap.String("rp", rpi.Name), zap.String("sub", sub.Name),
						zap.Strings("dest", sub.Destinations))
				}
			}
			s.writers[dbi.Name][rpi.Name] = writers
		})
	})
	s.lastModifiedID = s.client.GetMaxSubscriptionID()
}

func (s *SubscriberManager) WalkDatabases(fn func(db *meta.DatabaseInfo)) {
	dbs := s.client.Databases()
	for _, dbi := range dbs {
		fn(dbi)
	}
}

func (s *SubscriberManager) UpdateWriters() {
	s.WalkDatabases(func(dbi *meta.DatabaseInfo) {
		if _, ok := s.writers[dbi.Name]; !ok {
			s.writers[dbi.Name] = make(map[string][]SubscriberWriter)
		}
		dbi.WalkRetentionPolicy(func(rpi *meta.RetentionPolicyInfo) {
			changed := false
			s.lock.RLock()
			writers, ok := s.writers[dbi.Name][rpi.Name]
			s.lock.RUnlock()
			if !ok {
				writers = make([]SubscriberWriter, 0, len(rpi.Subscriptions))
				changed = true
			}
			// record origin subscription names in a set
			originSubs := make(map[string]struct{})
			for _, w := range writers {
				originSubs[w.Name()] = struct{}{}
			}
			// add new subscriptions
			for _, sub := range rpi.Subscriptions {
				if _, ok := originSubs[sub.Name]; !ok {
					writer, err := s.NewSubscriberWriter(dbi.Name, rpi.Name, sub.Name, sub.Mode, sub.Destinations)
					if err != nil {
						s.Logger.Error("fail to create subscriber", zap.String("db", dbi.Name), zap.String("rp", rpi.Name), zap.String("sub", sub.Name),
							zap.Strings("dest", sub.Destinations))
					} else {
						writers = append(writers, writer)
						writer.Start(s.config.WriteConcurrency, s.config.WriteBufferSize)
						s.Logger.Info("add new subscriber writer", zap.String("db", dbi.Name), zap.String("rp", rpi.Name), zap.String("sub", sub.Name),
							zap.Strings("dest", sub.Destinations))
						changed = true
					}
				}
				// remove all appeared subscription from the set
				// then rest names are of the subscriptions that should be removed
				delete(originSubs, sub.Name)
			}
			// if there is no subscriptions to remove,
			// just continue
			if len(originSubs) == 0 {
				if changed {
					s.lock.Lock()
					s.writers[dbi.Name][rpi.Name] = writers
					s.lock.Unlock()
				}
				return
			}
			position := 0
			length := len(writers)
			for i := 0; i < length; i++ {
				if _, ok := originSubs[writers[i].Name()]; !ok {
					writers[position] = writers[i]
					position++
				} else {
					writers[i].Stop()
					s.Logger.Info("remove subscriber writer", zap.String("db", dbi.Name), zap.String("rp", rpi.Name), zap.String("sub", writers[i].Name()))
				}
			}
			s.lock.Lock()
			s.writers[dbi.Name][rpi.Name] = writers[0:position]
			s.lock.Unlock()
		})
	})
}

func (s *SubscriberManager) Send(db, rp string, lineProtocal []byte) {
	if rp == "" {
		dbi, err := s.client.Database(db)
		if err != nil {
			s.Logger.Error("unknown database", zap.String("db", db))
		} else {
			rp = dbi.DefaultRetentionPolicy
		}
	}

	if writer, ok := s.writers[db][rp]; ok {
		for _, w := range writer {
			w.Write(lineProtocal)
		}
	}
}

func (s *SubscriberManager) SendColumn(db, rp, mst string, record array.Record) {
	if rp == "" {
		dbi, err := s.client.Database(db)
		if err != nil {
			s.Logger.Error("unknown database", zap.String("db", db))
		} else {
			rp = dbi.DefaultRetentionPolicy
		}
	}

	if writer, ok := s.writers[db][rp]; ok {
		for _, w := range writer {
			w.WriteColumn(mst, record)
		}
	}
}

func (s *SubscriberManager) StopAllWriters() {
	for _, db := range s.writers {
		for _, rp := range db {
			for _, writer := range rp {
				writer.Stop()
			}
		}
	}
}

func (s *SubscriberManager) Update() {
	for {
		ch := s.client.WaitForDataChanged()
		<-ch
		maxSubscriptionID := s.client.GetMaxSubscriptionID()
		if maxSubscriptionID > s.lastModifiedID {
			s.UpdateWriters()
			s.lastModifiedID = maxSubscriptionID
		}
	}
}

func NewSubscriberManager(c config.Subscriber, m MetaClient, l *logger.Logger) *SubscriberManager {
	m.Databases()
	s := &SubscriberManager{client: m, config: c, Logger: l}
	s.writers = make(map[string]map[string][]SubscriberWriter)
	return s
}
