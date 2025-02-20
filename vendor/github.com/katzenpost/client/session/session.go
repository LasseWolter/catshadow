// session.go - mixnet client session
// Copyright (C) 2018  David Stainton.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package session

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/katzenpost/client/config"
	cConstants "github.com/katzenpost/client/constants"
	"github.com/katzenpost/client/internal/pkiclient"
	"github.com/katzenpost/client/poisson"
	"github.com/katzenpost/client/utils"
	coreConstants "github.com/katzenpost/core/constants"
	"github.com/katzenpost/core/crypto/ecdh"
	"github.com/katzenpost/core/log"
	"github.com/katzenpost/core/pki"
	"github.com/katzenpost/core/sphinx"
	sConstants "github.com/katzenpost/core/sphinx/constants"
	"github.com/katzenpost/core/worker"
	"github.com/katzenpost/minclient"
	"gopkg.in/op/go-logging.v1"
)

// Session is the struct type that keeps state for a given session.
type Session struct {
	worker.Worker

	cfg       *config.Config
	pkiClient pki.Client
	minclient *minclient.Client
	log       *logging.Logger

	fatalErrCh chan error

	// λP
	pTimer *poisson.Fount
	// λD
	dTimer *poisson.Fount
	// λL
	lTimer *poisson.Fount

	linkKey   *ecdh.PrivateKey
	opCh      chan workerOp
	onlineAt  time.Time
	hasPKIDoc bool

	egressQueue EgressQueue

	waitSentChans map[[cConstants.MessageIDLength]byte]chan Event
	waitChans     map[[cConstants.MessageIDLength]byte]chan Event
	surbIDMap     map[[sConstants.SURBIDLength]byte]*Message
	messageIDMap  map[[cConstants.MessageIDLength]byte]*Message
	mapLock       *sync.Mutex

	decoyLoopTally uint64
}

// New establishes a session with provider using key.
// This method will block until session is connected to the Provider.
func New(ctx context.Context, fatalErrCh chan error, logBackend *log.Backend, user string, cfg *config.Config, linkKey *ecdh.PrivateKey) (*Session, error) {
	var err error

	// create a pkiclient for our own client lookups
	// AND create a pkiclient for minclient's use
	proxyCfg := cfg.UpstreamProxyConfig()
	pkiClient, err := cfg.NewPKIClient(logBackend, proxyCfg)
	if err != nil {
		return nil, err
	}

	// create a pkiclient for minclient's use
	pkiClient2, err := cfg.NewPKIClient(logBackend, proxyCfg)
	if err != nil {
		return nil, err
	}
	pkiCacheClient := pkiclient.New(pkiClient2)

	log := logBackend.GetLogger(fmt.Sprintf("%s@%s_c", user, cfg.Account.Provider))

	s := &Session{
		cfg:           cfg,
		linkKey:       linkKey,
		pkiClient:     pkiClient,
		log:           log,
		fatalErrCh:    fatalErrCh,
		opCh:          make(chan workerOp),
		waitChans:     make(map[[sConstants.SURBIDLength]byte]chan Event),
		waitSentChans: make(map[[cConstants.MessageIDLength]byte]chan Event),
	}
	s.surbIDMap = make(map[[sConstants.SURBIDLength]byte]*Message)
	s.messageIDMap = make(map[[cConstants.MessageIDLength]byte]*Message)
	s.mapLock = new(sync.Mutex)
	s.egressQueue = new(Queue)

	// Configure and bring up the minclient instance.
	clientCfg := &minclient.ClientConfig{
		User:                user,
		Provider:            cfg.Account.Provider,
		ProviderKeyPin:      cfg.Account.ProviderKeyPin,
		LinkKey:             s.linkKey,
		LogBackend:          logBackend,
		PKIClient:           pkiCacheClient,
		OnConnFn:            s.onConnection,
		OnMessageFn:         s.onMessage,
		OnACKFn:             s.onACK,
		OnDocumentFn:        s.onDocument,
		DialContextFn:       proxyCfg.ToDialContext("authority"),
		MessagePollInterval: 1 * time.Second,
		EnableTimeSync:      false, // Be explicit about it.
	}

	s.minclient, err = minclient.New(clientCfg)
	if err != nil {
		return nil, err
	}

	// block until we get the first PKI document
	// and then set our timers accordingly
	doc, err := s.awaitFirstPKIDoc(ctx)
	if err != nil {
		return nil, err
	}
	s.setTimers(doc)
	s.Go(s.worker)
	return s, nil
}

func (s *Session) awaitFirstPKIDoc(ctx context.Context) (*pki.Document, error) {
	for {
		var qo workerOp
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-s.HaltCh():
			s.log.Debugf("Terminating gracefully.")
			return nil, errors.New("Terminating gracefully.")
		case <-time.After(time.Duration(s.cfg.Debug.InitialMaxPKIRetrievalDelay) * time.Second):
			return nil, errors.New("Timeout failure awaiting first PKI document.")
		case qo = <-s.opCh:
		}
		switch op := qo.(type) {
		case opNewDocument:
			// Determine if PKI doc is valid. If not then abort.
			err := s.isDocValid(op.doc)
			if err != nil {
				s.log.Errorf("Aborting, PKI doc is not valid for the Loopix decoy traffic use case: %v", err)
				err := fmt.Errorf("Aborting, PKI doc is not valid for the Loopix decoy traffic use case: %v", err)
				s.fatalErrCh <- err
				return nil, err
			}
			return op.doc, nil
		default:

			continue
		}
	}
}

// GetService returns a randomly selected service
// matching the specified service name
func (s *Session) GetService(serviceName string) (*utils.ServiceDescriptor, error) {
	doc := s.minclient.CurrentDocument()
	if doc == nil {
		return nil, errors.New("pki doc is nil")
	}
	serviceDescriptors := utils.FindServices(serviceName, doc)
	if len(serviceDescriptors) == 0 {
		return nil, errors.New("GetService failure, service not found in pki doc.")
	}
	return &serviceDescriptors[mrand.Intn(len(serviceDescriptors))], nil
}

// OnConnection will be called by the minclient api
// upon connecting to the Provider
func (s *Session) onConnection(err error) {
	if err == nil {
		s.opCh <- opConnStatusChanged{
			isConnected: true,
		}
	}
}

// OnMessage will be called by the minclient api
// upon receiving a message
func (s *Session) onMessage(ciphertextBlock []byte) error {
	s.log.Debugf("OnMessage")
	return nil
}

func (s *Session) incrementDecoyLoopTally() {
	atomic.AddUint64(&s.decoyLoopTally, 1)
}

func (s *Session) decrementDecoyLoopTally() {
	atomic.AddUint64(&s.decoyLoopTally, ^uint64(0))
}

// OnACK is called by the minclient api when we receive a SURB reply message.
func (s *Session) onACK(surbID *[sConstants.SURBIDLength]byte, ciphertext []byte) error {
	idStr := fmt.Sprintf("[%v]", hex.EncodeToString(surbID[:]))
	s.log.Infof("OnACK with SURBID %x", idStr)
	s.mapLock.Lock()
	defer s.mapLock.Unlock()
	msg, ok := s.surbIDMap[*surbID]
	if !ok {
		s.log.Debug("Strange, received reply with unexpected SURBID")
		return nil
	}
	plaintext, err := sphinx.DecryptSURBPayload(ciphertext, msg.Key)
	if err != nil {
		s.log.Infof("SURB Reply decryption failure: %s", err)
		return err
	}
	if len(plaintext) != coreConstants.ForwardPayloadLength {
		s.log.Warningf("Discarding SURB %v: Invalid payload size: %v", idStr, len(plaintext))
		return nil
	}
	if msg.WithSURB && msg.IsDecoy {
		_, ok := s.surbIDMap[*surbID]
		if ok {
			s.decrementDecoyLoopTally()
			delete(s.surbIDMap, *surbID)
			return nil
		}
		s.log.Warning("Reply is from an unknown Decoy Loop Message.")
		return nil
	}
	switch msg.SURBType {
	case cConstants.SurbTypeKaetzchen, cConstants.SurbTypeInternal:
		waitCh, ok := s.waitChans[*msg.ID]
		if ok {
			select {
			case waitCh <- &MessageReplyEvent{
				MessageID: msg.ID,
				Payload:   plaintext[2:],
				Err:       nil,
			}:
			case <-time.After(3 * time.Second):
				s.log.Warning("Message Reply Event send timeout failure.")
			}
		} else {
			s.log.Warning("Error, failure to find wait chan in waitChans map for that message ID.")
		}
	default:
		s.log.Warningf("Discarding SURB %v: Unknown type: 0x%02x", idStr, msg.SURBType)
	}
	return nil
}

func (s *Session) onDocument(doc *pki.Document) {
	s.log.Debugf("onDocument(): Epoch %v", doc.Epoch)
	s.hasPKIDoc = true
	s.opCh <- opNewDocument{
		doc: doc,
	}
}

func (s *Session) GetPandaConfig() *config.Panda {
	return s.cfg.Panda
}
