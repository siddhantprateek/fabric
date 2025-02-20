/*
Copyright IBM Corp All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package raft

import (
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	docker "github.com/fsouza/go-dockerclient"
	"github.com/golang/protobuf/proto"
	"github.com/hyperledger/fabric-protos-go/common"
	protosorderer "github.com/hyperledger/fabric-protos-go/orderer"
	protosraft "github.com/hyperledger/fabric-protos-go/orderer/etcdraft"
	"github.com/hyperledger/fabric/integration/nwo"
	"github.com/hyperledger/fabric/integration/nwo/commands"
	"github.com/hyperledger/fabric/integration/ordererclient"
	"github.com/hyperledger/fabric/protoutil"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gbytes"
	"github.com/onsi/gomega/gexec"
	"github.com/tedsuo/ifrit"
	ginkgomon "github.com/tedsuo/ifrit/ginkgomon_v2"
)

var _ = Describe("Solo2RaftMigration", func() {
	var (
		testDir string
		client  *docker.Client
		network *nwo.Network

		process                ifrit.Process
		brokerProc             ifrit.Process
		o1Proc, o2Proc, o3Proc ifrit.Process

		o1Runner, o2Runner *ginkgomon.Runner
	)

	BeforeEach(func() {
		var err error
		testDir, err = ioutil.TempDir("", "solo2raft-migration")
		Expect(err).NotTo(HaveOccurred())

		client, err = docker.NewClientFromEnv()
		Expect(err).NotTo(HaveOccurred())
	})

	AfterEach(func() {
		if process != nil {
			process.Signal(syscall.SIGTERM)
			Eventually(process.Wait(), network.EventuallyTimeout).Should(Receive())
		}

		for _, oProc := range []ifrit.Process{o1Proc, o2Proc, o3Proc} {
			if oProc != nil {
				oProc.Signal(syscall.SIGTERM)
				Eventually(oProc.Wait(), network.EventuallyTimeout).Should(Receive())
			}
		}

		if brokerProc != nil {
			brokerProc.Signal(syscall.SIGTERM)
			Eventually(brokerProc.Wait(), network.EventuallyTimeout).Should(Receive())
		}

		if network != nil {
			network.Cleanup()
		}

		_ = os.RemoveAll(testDir)
	})

	// These tests execute the migration config updates on a solo based system, restart the orderer onto a Raft-based
	// system, and verifies that the newly restarted orderer (single node) cluster performs as expected.
	Describe("Solo to Raft migration", func() {
		var (
			orderer                                  *nwo.Orderer
			peer                                     *nwo.Peer
			syschannel, channel1, channel2, channel3 string
			raftMetadata                             []byte
		)

		BeforeEach(func() {
			network = nwo.New(solo2RaftMultiChannel(), testDir, client, StartPort(), components)
			network.GenerateConfigTree()
			network.Bootstrap()

			orderer = network.Orderer("orderer")
			peer = network.Peer("Org1", "peer0")

			o1Runner = network.OrdererRunner(orderer)

			o1Proc = ifrit.Invoke(o1Runner)
			Eventually(o1Proc.Ready(), network.EventuallyTimeout).Should(BeClosed())

			raftMetadata = prepareRaftMetadata(network)

			syschannel = network.SystemChannel.Name
			channel1 = "testchannel1"
			channel2 = "testchannel2"
			channel3 = "testchannel3"

			By("Create & join first channel, deploy and invoke chaincode")
			network.CreateChannel(channel1, orderer, peer)
		})

		// This test executes the "green path" migration config updates on solo based system with a system channel
		// and a standard channel, and verifies that these config updates have the desired effect.
		//
		// The green path is entering maintenance mode, and then changing the consensus type.
		// In maintenance mode we check that channel creation is blocked and that normal transactions are blocked.
		//
		// We also check that after entering maintenance mode, we can exit it without making any changes - the "abort path".
		It("executes solo2raft green path", func() {
			//=== The abort path ======================================================================================
			//=== Step 1: Config update on system channel, MAINTENANCE ===
			By("1) Config update on system channel, State=MAINTENANCE, enter maintenance-mode")
			config, updatedConfig := prepareTransition(network, peer, orderer, syschannel,
				"solo", protosorderer.ConsensusType_STATE_NORMAL,
				"solo", nil, protosorderer.ConsensusType_STATE_MAINTENANCE)
			nwo.UpdateOrdererConfig(network, orderer, syschannel, config, updatedConfig, peer, orderer)

			By("1) Verify: system channel1 config changed")
			sysStartBlockNum := nwo.CurrentConfigBlockNumber(network, peer, orderer, syschannel)
			Expect(sysStartBlockNum).ToNot(Equal(0))
			config = nwo.GetConfig(network, peer, orderer, syschannel)
			consensusTypeValue := extractOrdererConsensusType(config)
			validateConsensusTypeValue(consensusTypeValue, "solo", protosorderer.ConsensusType_STATE_MAINTENANCE)

			By("1) Verify: new channels cannot be created")
			exitCode := network.CreateChannelExitCode(channel2, orderer, peer)
			Expect(exitCode).ToNot(Equal(0))

			//=== Step 2: Config update on standard channel, MAINTENANCE ===
			By("2) Config update on standard channel, State=MAINTENANCE, enter maintenance-mode")
			config, updatedConfig = prepareTransition(network, peer, orderer, channel1,
				"solo", protosorderer.ConsensusType_STATE_NORMAL,
				"solo", nil, protosorderer.ConsensusType_STATE_MAINTENANCE)
			nwo.UpdateOrdererConfig(network, orderer, channel1, config, updatedConfig, peer, orderer)

			By("2) Verify: standard channel config changed")
			std1EntryBlockNum := nwo.CurrentConfigBlockNumber(network, peer, orderer, channel1)
			Expect(std1EntryBlockNum).ToNot(Equal(0))
			config = nwo.GetConfig(network, peer, orderer, channel1)
			consensusTypeValue = extractOrdererConsensusType(config)
			validateConsensusTypeValue(consensusTypeValue, "solo", protosorderer.ConsensusType_STATE_MAINTENANCE)

			By("2) Verify: Normal TX's on standard channel are blocked")
			assertTxFailed(network, orderer, channel1)

			// In maintenance mode deliver requests are open to those entities that satisfy the /Channel/Orderer/Readers policy
			By("2) Verify: delivery request from peer is blocked")
			err := checkPeerDeliverRequest(orderer, peer, network, channel1)
			Expect(err).To(MatchError(errors.New("FORBIDDEN")))

			//=== Step 3: config update on system channel, State=NORMAL, abort ===
			By("3) Config update on system channel, State=NORMAL, exit maintenance-mode - abort path")
			config, updatedConfig = prepareTransition(network, peer, orderer, syschannel,
				"solo", protosorderer.ConsensusType_STATE_MAINTENANCE,
				"solo", nil, protosorderer.ConsensusType_STATE_NORMAL)
			nwo.UpdateOrdererConfig(network, orderer, syschannel, config, updatedConfig, peer, orderer)

			By("3) Verify: system channel config changed")
			sysBlockNum := nwo.CurrentConfigBlockNumber(network, peer, orderer, syschannel)
			Expect(sysBlockNum).To(Equal(sysStartBlockNum + 1))

			By("3) Verify: create new channel, executing transaction")
			network.CreateChannel(channel2, orderer, peer)

			assertBlockCreation(network, orderer, peer, channel2, 1)

			By("3) Verify: delivery request from peer is not blocked on new channel")
			err = checkPeerDeliverRequest(orderer, peer, network, channel2)
			Expect(err).NotTo(HaveOccurred())

			//=== Step 4: config update on standard channel, State=NORMAL, abort ===
			By("4) Config update on standard channel, State=NORMAL, exit maintenance-mode - abort path")
			config, updatedConfig = prepareTransition(network, peer, orderer, channel1,
				"solo", protosorderer.ConsensusType_STATE_MAINTENANCE,
				"solo", nil, protosorderer.ConsensusType_STATE_NORMAL)
			nwo.UpdateOrdererConfig(network, orderer, channel1, config, updatedConfig, peer, orderer)

			By("4) Verify: standard channel config changed")
			std1BlockNum := nwo.CurrentConfigBlockNumber(network, peer, orderer, channel1)
			Expect(std1BlockNum).To(Equal(std1EntryBlockNum + 1))

			By("4) Verify: standard channel delivery requests from peer unblocked")
			err = checkPeerDeliverRequest(orderer, peer, network, channel1)
			Expect(err).NotTo(HaveOccurred())

			By("4) Verify: Normal TX's on standard channel are permitted again")
			assertBlockCreation(network, orderer, nil, channel1, 3)

			//=== The green path ======================================================================================
			//=== Step 5: Config update on system channel, MAINTENANCE, again ===
			By("5) Config update on system channel, State=MAINTENANCE, enter maintenance-mode again")
			config, updatedConfig = prepareTransition(network, peer, orderer, syschannel,
				"solo", protosorderer.ConsensusType_STATE_NORMAL,
				"solo", nil, protosorderer.ConsensusType_STATE_MAINTENANCE)
			nwo.UpdateOrdererConfig(network, orderer, syschannel, config, updatedConfig, peer, orderer)

			By("5) Verify: system channel config changed")
			sysStartBlockNum = nwo.CurrentConfigBlockNumber(network, peer, orderer, syschannel)
			Expect(sysStartBlockNum).ToNot(Equal(0))
			config = nwo.GetConfig(network, peer, orderer, syschannel)
			consensusTypeValue = extractOrdererConsensusType(config)
			validateConsensusTypeValue(consensusTypeValue, "solo", protosorderer.ConsensusType_STATE_MAINTENANCE)

			//=== Step 6: Config update on standard channel1, MAINTENANCE, again ===
			By("6) Config update on standard channel1, State=MAINTENANCE, enter maintenance-mode again")
			config, updatedConfig = prepareTransition(network, peer, orderer, channel1,
				"solo", protosorderer.ConsensusType_STATE_NORMAL,
				"solo", nil, protosorderer.ConsensusType_STATE_MAINTENANCE)
			nwo.UpdateOrdererConfig(network, orderer, channel1, config, updatedConfig, peer, orderer)

			By("6) Verify: standard channel config changed")
			std1EntryBlockNum = nwo.CurrentConfigBlockNumber(network, peer, orderer, channel1)
			Expect(std1EntryBlockNum).ToNot(Equal(0))

			config = nwo.GetConfig(network, peer, orderer, channel1)
			consensusTypeValue = extractOrdererConsensusType(config)
			validateConsensusTypeValue(consensusTypeValue, "solo", protosorderer.ConsensusType_STATE_MAINTENANCE)

			By("6) Verify: delivery request from peer is blocked")
			err = checkPeerDeliverRequest(orderer, peer, network, channel1)
			Expect(err).To(MatchError(errors.New("FORBIDDEN")))

			By("6) Verify: Normal TX's on standard channel are blocked")
			assertTxFailed(network, orderer, channel1)

			//=== Step 7: Config update on standard channel2, MAINTENANCE ===
			By("7) Config update on standard channel2, State=MAINTENANCE, enter maintenance-mode again")
			config, updatedConfig = prepareTransition(network, peer, orderer, channel2,
				"solo", protosorderer.ConsensusType_STATE_NORMAL,
				"solo", nil, protosorderer.ConsensusType_STATE_MAINTENANCE)
			nwo.UpdateOrdererConfig(network, orderer, channel2, config, updatedConfig, peer, orderer)

			By("7) Verify: standard channel config changed")
			std2EntryBlockNum := nwo.CurrentConfigBlockNumber(network, peer, orderer, channel2)
			Expect(std2EntryBlockNum).ToNot(Equal(0))

			config = nwo.GetConfig(network, peer, orderer, channel2)
			consensusTypeValue = extractOrdererConsensusType(config)
			validateConsensusTypeValue(consensusTypeValue, "solo", protosorderer.ConsensusType_STATE_MAINTENANCE)

			By("7) Verify: delivery request from peer is blocked")
			err = checkPeerDeliverRequest(orderer, peer, network, channel2)
			Expect(err).To(MatchError(errors.New("FORBIDDEN")))

			By("7) Verify: Normal TX's on standard channel are blocked")
			assertTxFailed(network, orderer, channel2)

			//=== Step 8: config update on system channel, State=MAINTENANCE, type=etcdraft ===
			By("8) Config update on system channel, State=MAINTENANCE, type=etcdraft")
			config, updatedConfig = prepareTransition(network, peer, orderer, syschannel,
				"solo", protosorderer.ConsensusType_STATE_MAINTENANCE,
				"etcdraft", raftMetadata, protosorderer.ConsensusType_STATE_MAINTENANCE)
			nwo.UpdateOrdererConfig(network, orderer, syschannel, config, updatedConfig, peer, orderer)

			By("8) Verify: system channel config changed")
			sysBlockNum = nwo.CurrentConfigBlockNumber(network, peer, orderer, syschannel)
			Expect(sysBlockNum).To(Equal(sysStartBlockNum + 1))

			By("8) Verify: new channels cannot be created")
			exitCode = network.CreateChannelExitCode(channel3, orderer, peer)
			Expect(exitCode).ToNot(Equal(0))

			//=== Step 9: config update on standard channel1, State=MAINTENANCE, type=etcdraft ===
			By("9) Config update on standard channel1, State=MAINTENANCE, type=etcdraft")
			config, updatedConfig = prepareTransition(network, peer, orderer, channel1,
				"solo", protosorderer.ConsensusType_STATE_MAINTENANCE,
				"etcdraft", raftMetadata, protosorderer.ConsensusType_STATE_MAINTENANCE)
			nwo.UpdateOrdererConfig(network, orderer, channel1, config, updatedConfig, peer, orderer)

			By("9) Verify: standard channel config changed")
			std1BlockNum = nwo.CurrentConfigBlockNumber(network, peer, orderer, channel1)
			Expect(std1BlockNum).To(Equal(std1EntryBlockNum + 1))

			By("9) Verify: delivery request from peer is blocked")
			err = checkPeerDeliverRequest(orderer, peer, network, channel1)
			Expect(err).To(MatchError(errors.New("FORBIDDEN")))

			By("9) Verify: Normal TX's on standard channel are blocked")
			assertTxFailed(network, orderer, channel1)

			//=== Step 10: config update on standard channel2, State=MAINTENANCE, type=etcdraft ===
			By("10) Config update on standard channel2, State=MAINTENANCE, type=etcdraft")
			config, updatedConfig = prepareTransition(network, peer, orderer, channel2,
				"solo", protosorderer.ConsensusType_STATE_MAINTENANCE,
				"etcdraft", raftMetadata, protosorderer.ConsensusType_STATE_MAINTENANCE)
			nwo.UpdateOrdererConfig(network, orderer, channel2, config, updatedConfig, peer, orderer)

			By("10) Verify: standard channel config changed")
			std2BlockNum := nwo.CurrentConfigBlockNumber(network, peer, orderer, channel2)
			Expect(std2BlockNum).To(Equal(std2EntryBlockNum + 1))

			By("10) Verify: delivery request from peer is blocked")
			err = checkPeerDeliverRequest(orderer, peer, network, channel2)
			Expect(err).To(MatchError(errors.New("FORBIDDEN")))

			By("10) Verify: Normal TX's on standard channel are blocked")
			assertTxFailed(network, orderer, channel2)
		})

		// This test executes the migration flow and checks that forbidden transitions are rejected.
		// These transitions are enforced by the maintenance filter:
		// - Entry to & exit from maintenance mode can only change ConsensusType.State.
		// - In maintenance mode one can only change ConsensusType.Type & ConsensusType.Metadata.
		// - ConsensusType.Type can only change from "solo" to "etcdraft", and only in maintenance mode.
		It("executes solo2raft forbidden transitions", func() {
			//=== Step 1: ===
			By("1) Config update on system channel, changing both ConsensusType State & Type is forbidden")
			assertTransitionFailed(network, peer, orderer, syschannel,
				"solo", protosorderer.ConsensusType_STATE_NORMAL,
				"etcdraft", raftMetadata, protosorderer.ConsensusType_STATE_MAINTENANCE)

			//=== Step 2: ===
			By("2) Config update on standard channel, changing both ConsensusType State & Type is forbidden")
			assertTransitionFailed(network, peer, orderer, channel1,
				"solo", protosorderer.ConsensusType_STATE_NORMAL,
				"etcdraft", raftMetadata, protosorderer.ConsensusType_STATE_MAINTENANCE)

			//=== Step 3: ===
			By("3) Config update on system channel, changing both ConsensusType State & some other value is forbidden")
			config, updatedConfig := prepareTransition(network, peer, orderer, syschannel,
				"solo", protosorderer.ConsensusType_STATE_NORMAL,
				"solo", nil, protosorderer.ConsensusType_STATE_MAINTENANCE)
			updateConfigWithBatchTimeout(updatedConfig)
			updateOrdererConfigFailed(network, orderer, syschannel, config, updatedConfig, peer, orderer)

			//=== Step 4: ===
			By("4) Config update on standard channel, both ConsensusType State & some other value is forbidden")
			config, updatedConfig = prepareTransition(network, peer, orderer, channel1,
				"solo", protosorderer.ConsensusType_STATE_NORMAL,
				"solo", nil, protosorderer.ConsensusType_STATE_MAINTENANCE)
			updateConfigWithBatchTimeout(updatedConfig)
			updateOrdererConfigFailed(network, orderer, channel1, config, updatedConfig, peer, orderer)

			//=== Step 5: ===
			By("5) Config update on system channel, State=MAINTENANCE, enter maintenance-mode")
			config, updatedConfig = prepareTransition(network, peer, orderer, syschannel,
				"solo", protosorderer.ConsensusType_STATE_NORMAL,
				"solo", nil, protosorderer.ConsensusType_STATE_MAINTENANCE)
			nwo.UpdateOrdererConfig(network, orderer, syschannel, config, updatedConfig, peer, orderer)

			By("5) Verify: system channel config changed")
			sysStartBlockNum := nwo.CurrentConfigBlockNumber(network, peer, orderer, syschannel)
			Expect(sysStartBlockNum).ToNot(Equal(0))
			config = nwo.GetConfig(network, peer, orderer, syschannel)
			consensusTypeValue := extractOrdererConsensusType(config)
			validateConsensusTypeValue(consensusTypeValue, "solo", protosorderer.ConsensusType_STATE_MAINTENANCE)

			//=== Step 6: ===
			By("6) Config update on standard channel, State=MAINTENANCE, enter maintenance-mode")
			config, updatedConfig = prepareTransition(network, peer, orderer, channel1,
				"solo", protosorderer.ConsensusType_STATE_NORMAL,
				"solo", nil, protosorderer.ConsensusType_STATE_MAINTENANCE)
			nwo.UpdateOrdererConfig(network, orderer, channel1, config, updatedConfig, peer, orderer)

			By("6) Verify: standard channel config changed")
			std1StartBlockNum := nwo.CurrentConfigBlockNumber(network, peer, orderer, channel1)
			Expect(std1StartBlockNum).ToNot(Equal(0))
			config = nwo.GetConfig(network, peer, orderer, channel1)
			consensusTypeValue = extractOrdererConsensusType(config)
			validateConsensusTypeValue(consensusTypeValue, "solo", protosorderer.ConsensusType_STATE_MAINTENANCE)

			//=== Step 7: ===
			By("7) Config update on system channel, change ConsensusType.Type to unsupported type, forbidden")
			assertTransitionFailed(network, peer, orderer, syschannel,
				"solo", protosorderer.ConsensusType_STATE_MAINTENANCE,
				"melville", nil, protosorderer.ConsensusType_STATE_MAINTENANCE)

			//=== Step 8: ===
			By("8) Config update on standard channel, change ConsensusType.Type to unsupported type, forbidden")
			assertTransitionFailed(network, peer, orderer, channel1,
				"solo", protosorderer.ConsensusType_STATE_MAINTENANCE,
				"hesse", nil, protosorderer.ConsensusType_STATE_MAINTENANCE)

			//=== Step 9: ===
			By("9) Config update on system channel, change ConsensusType.Type and State, forbidden")
			assertTransitionFailed(network, peer, orderer, syschannel,
				"solo", protosorderer.ConsensusType_STATE_MAINTENANCE,
				"etcdraft", raftMetadata, protosorderer.ConsensusType_STATE_NORMAL)

			//=== Step 10: ===
			By("10) Config update on standard channel, change ConsensusType.Type and State, forbidden")
			assertTransitionFailed(network, peer, orderer, channel1,
				"solo", protosorderer.ConsensusType_STATE_MAINTENANCE,
				"etcdraft", raftMetadata, protosorderer.ConsensusType_STATE_NORMAL)

			//=== Step 11: ===
			By("11) Config update on system channel, changing both ConsensusType.Type and other value is permitted")
			config, updatedConfig = prepareTransition(network, peer, orderer, syschannel,
				"solo", protosorderer.ConsensusType_STATE_MAINTENANCE,
				"etcdraft", raftMetadata, protosorderer.ConsensusType_STATE_MAINTENANCE)
			updateConfigWithBatchTimeout(updatedConfig)
			nwo.UpdateOrdererConfig(network, orderer, syschannel, config, updatedConfig, peer, orderer)

			By("11) Verify: system channel config changed")
			sysBlockNum := nwo.CurrentConfigBlockNumber(network, peer, orderer, syschannel)
			Expect(sysBlockNum).To(Equal(sysStartBlockNum + 1))
			config = nwo.GetConfig(network, peer, orderer, syschannel)
			consensusTypeValue = extractOrdererConsensusType(config)
			validateConsensusTypeValue(consensusTypeValue, "etcdraft", protosorderer.ConsensusType_STATE_MAINTENANCE)

			//=== Step 12: ===
			By("12) Config update on standard channel, changing both ConsensusType.Type and other value is permitted")
			config, updatedConfig = prepareTransition(network, peer, orderer, channel1,
				"solo", protosorderer.ConsensusType_STATE_MAINTENANCE,
				"etcdraft", raftMetadata, protosorderer.ConsensusType_STATE_MAINTENANCE)
			updateConfigWithBatchTimeout(updatedConfig)
			nwo.UpdateOrdererConfig(network, orderer, channel1, config, updatedConfig, peer, orderer)

			By("12) Verify: standard channel config changed")
			std1BlockNum := nwo.CurrentConfigBlockNumber(network, peer, orderer, channel1)
			Expect(std1BlockNum).To(Equal(std1StartBlockNum + 1))
			config = nwo.GetConfig(network, peer, orderer, channel1)
			consensusTypeValue = extractOrdererConsensusType(config)
			validateConsensusTypeValue(consensusTypeValue, "etcdraft", protosorderer.ConsensusType_STATE_MAINTENANCE)

			//=== Step 13: ===
			By("13) Config update on system channel, changing value other than ConsensusType.Type is permitted")
			config = nwo.GetConfig(network, peer, orderer, syschannel)
			consensusTypeValue = extractOrdererConsensusType(config)
			validateConsensusTypeValue(consensusTypeValue, "etcdraft", protosorderer.ConsensusType_STATE_MAINTENANCE)
			updatedConfig = proto.Clone(config).(*common.Config)
			updateConfigWithBatchTimeout(updatedConfig)
			nwo.UpdateOrdererConfig(network, orderer, syschannel, config, updatedConfig, peer, orderer)

			By("13) Verify: system channel config changed")
			sysBlockNum = nwo.CurrentConfigBlockNumber(network, peer, orderer, syschannel)
			Expect(sysBlockNum).To(Equal(sysStartBlockNum + 2))

			//=== Step 14: ===
			By("14) Config update on standard channel, changing value other than ConsensusType.Type is permitted")
			config = nwo.GetConfig(network, peer, orderer, channel1)
			consensusTypeValue = extractOrdererConsensusType(config)
			validateConsensusTypeValue(consensusTypeValue, "etcdraft", protosorderer.ConsensusType_STATE_MAINTENANCE)
			updatedConfig = proto.Clone(config).(*common.Config)
			updateConfigWithBatchTimeout(updatedConfig)
			nwo.UpdateOrdererConfig(network, orderer, channel1, config, updatedConfig, peer, orderer)

			By("14) Verify: standard channel config changed")
			std1BlockNum = nwo.CurrentConfigBlockNumber(network, peer, orderer, channel1)
			Expect(std1BlockNum).To(Equal(std1StartBlockNum + 2))

			//=== Step 15: ===
			By("15) Config update on system channel, changing both ConsensusType State & some other value is forbidden")
			config, updatedConfig = prepareTransition(network, peer, orderer, syschannel,
				"etcdraft", protosorderer.ConsensusType_STATE_MAINTENANCE,
				"etcdraft", raftMetadata, protosorderer.ConsensusType_STATE_NORMAL)
			updateConfigWithBatchTimeout(updatedConfig)
			updateOrdererConfigFailed(network, orderer, syschannel, config, updatedConfig, peer, orderer)

			//=== Step 16: ===
			By("16) Config update on standard channel, both ConsensusType State & some other value is forbidden")
			config, updatedConfig = prepareTransition(network, peer, orderer, channel1,
				"etcdraft", protosorderer.ConsensusType_STATE_MAINTENANCE,
				"etcdraft", raftMetadata, protosorderer.ConsensusType_STATE_NORMAL)
			updateConfigWithBatchTimeout(updatedConfig)
			updateOrdererConfigFailed(network, orderer, channel1, config, updatedConfig, peer, orderer)
		})

		It("executes bootstrap to raft - single node", func() {
			//=== Step 1: Config update on system channel, MAINTENANCE ===
			By("1) Config update on system channel, State=MAINTENANCE")
			config, updatedConfig := prepareTransition(network, peer, orderer, syschannel,
				"solo", protosorderer.ConsensusType_STATE_NORMAL,
				"solo", nil, protosorderer.ConsensusType_STATE_MAINTENANCE)
			nwo.UpdateOrdererConfig(network, orderer, syschannel, config, updatedConfig, peer, orderer)

			By("1) Verify: system channel config changed")
			sysStartBlockNum := nwo.CurrentConfigBlockNumber(network, peer, orderer, syschannel)
			Expect(sysStartBlockNum).ToNot(Equal(0))

			config = nwo.GetConfig(network, peer, orderer, syschannel)
			consensusTypeValue := extractOrdererConsensusType(config)
			validateConsensusTypeValue(consensusTypeValue, "solo", protosorderer.ConsensusType_STATE_MAINTENANCE)

			By("1) Verify: new channels cannot be created")
			exitCode := network.CreateChannelExitCode(channel2, orderer, peer)
			Expect(exitCode).ToNot(Equal(0))

			//=== Step 2: Config update on standard channel, MAINTENANCE ===
			By("2) Config update on standard channel, State=MAINTENANCE")
			config, updatedConfig = prepareTransition(network, peer, orderer, channel1,
				"solo", protosorderer.ConsensusType_STATE_NORMAL,
				"solo", nil, protosorderer.ConsensusType_STATE_MAINTENANCE)
			nwo.UpdateOrdererConfig(network, orderer, channel1, config, updatedConfig, peer, orderer)

			By("2) Verify: standard channel config changed")
			chan1StartBlockNum := nwo.CurrentConfigBlockNumber(network, peer, orderer, channel1)
			Expect(chan1StartBlockNum).ToNot(Equal(0))

			config = nwo.GetConfig(network, peer, orderer, channel1)
			consensusTypeValue = extractOrdererConsensusType(config)
			validateConsensusTypeValue(consensusTypeValue, "solo", protosorderer.ConsensusType_STATE_MAINTENANCE)

			By("2) Verify: Normal TX's on standard channel are blocked")
			assertTxFailed(network, orderer, channel1)

			// In maintenance mode deliver requests are open to those entities that satisfy the /Channel/Orderer/Readers policy
			By("2) Verify: delivery request from peer is blocked")
			err := checkPeerDeliverRequest(orderer, peer, network, channel1)
			Expect(err).To(MatchError(errors.New("FORBIDDEN")))

			//=== Step 3: config update on system channel, State=MAINTENANCE, type=etcdraft ===
			By("3) Config update on system channel, State=MAINTENANCE, type=etcdraft")
			config, updatedConfig = prepareTransition(network, peer, orderer, syschannel,
				"solo", protosorderer.ConsensusType_STATE_MAINTENANCE,
				"etcdraft", raftMetadata, protosorderer.ConsensusType_STATE_MAINTENANCE)
			nwo.UpdateOrdererConfig(network, orderer, syschannel, config, updatedConfig, peer, orderer)

			By("3) Verify: system channel config changed")
			sysBlockNum := nwo.CurrentConfigBlockNumber(network, peer, orderer, syschannel)
			Expect(sysBlockNum).To(Equal(sysStartBlockNum + 1))

			//=== Step 4: config update on standard channel, State=MAINTENANCE, type=etcdraft ===
			By("4) Config update on standard channel, State=MAINTENANCE, type=etcdraft")
			config, updatedConfig = prepareTransition(network, peer, orderer, channel1,
				"solo", protosorderer.ConsensusType_STATE_MAINTENANCE,
				"etcdraft", raftMetadata, protosorderer.ConsensusType_STATE_MAINTENANCE)
			nwo.UpdateOrdererConfig(network, orderer, channel1, config, updatedConfig, peer, orderer)

			By("4) Verify: standard channel config changed")
			chan1BlockNum := nwo.CurrentConfigBlockNumber(network, peer, orderer, channel1)
			Expect(chan1BlockNum).To(Equal(chan1StartBlockNum + 1))

			//=== Step 5: kill ===
			By("5) killing orderer1")
			o1Proc.Signal(syscall.SIGKILL)
			Eventually(o1Proc.Wait(), network.EventuallyTimeout).Should(Receive(MatchError("exit status 137")))

			//=== Step 6: restart ===
			By("6) restarting orderer1")
			network.Consensus.Type = "etcdraft"

			o1Runner = network.OrdererRunner(orderer)
			o1Proc = ifrit.Invoke(o1Runner)

			Eventually(o1Proc.Ready(), network.EventuallyTimeout).Should(BeClosed())

			assertBlockReception(
				map[string]int{
					syschannel: int(sysBlockNum),
					channel1:   int(chan1BlockNum),
				},
				[]*nwo.Orderer{orderer},
				peer,
				network,
			)

			Eventually(o1Runner.Err(), network.EventuallyTimeout, time.Second).Should(gbytes.Say("Raft leader changed: 0 -> "))
			Eventually(o1Proc.Ready(), network.EventuallyTimeout).Should(BeClosed())

			By("7) System channel still in maintenance, State=MAINTENANCE, cannot create new channels")
			exitCode = network.CreateChannelExitCode(channel2, orderer, peer)
			Expect(exitCode).ToNot(Equal(0))

			By("8) Standard channel still in maintenance, State=MAINTENANCE, normal TX's blocked, delivery to peers blocked")
			assertTxFailed(network, orderer, channel1)

			err = checkPeerDeliverRequest(orderer, peer, network, channel1)
			Expect(err).To(MatchError(errors.New("FORBIDDEN")))

			By("9) Release - executing config transaction on system channel with restarted orderer")
			config, updatedConfig = prepareTransition(network, peer, orderer, syschannel,
				"etcdraft", protosorderer.ConsensusType_STATE_MAINTENANCE,
				"etcdraft", raftMetadata, protosorderer.ConsensusType_STATE_NORMAL)
			nwo.UpdateOrdererConfig(network, orderer, syschannel, config, updatedConfig, peer, orderer)

			By("9) Verify: system channel config changed")
			sysBlockNum = nwo.CurrentConfigBlockNumber(network, peer, orderer, syschannel)
			Expect(sysBlockNum).To(Equal(sysStartBlockNum + 2))

			By("10) Release - executing config transaction on standard channel with restarted orderer")
			config, updatedConfig = prepareTransition(network, peer, orderer, channel1,
				"etcdraft", protosorderer.ConsensusType_STATE_MAINTENANCE,
				"etcdraft", raftMetadata, protosorderer.ConsensusType_STATE_NORMAL)
			nwo.UpdateOrdererConfig(network, orderer, channel1, config, updatedConfig, peer, orderer)

			By("10) Verify: standard channel config changed")
			chan1BlockNum = nwo.CurrentConfigBlockNumber(network, peer, orderer, channel1)
			Expect(chan1BlockNum).To(Equal(chan1StartBlockNum + 2))

			By("11) Executing transaction on standard channel with restarted orderer")
			assertBlockCreation(network, orderer, peer, channel1, chan1StartBlockNum+3)
			assertBlockCreation(network, orderer, nil, channel1, chan1StartBlockNum+4)

			By("12) Create new channel, executing transaction with restarted orderer")
			network.CreateChannel(channel2, orderer, peer)

			assertBlockCreation(network, orderer, peer, channel2, 1)
			assertBlockCreation(network, orderer, nil, channel2, 2)

			By("13) Extending the network configuration to add a new orderer")
			// Add another orderer
			orderer2 := &nwo.Orderer{
				Name:         "orderer2",
				Organization: "OrdererOrg",
			}
			ports := nwo.Ports{}
			for _, portName := range nwo.OrdererPortNames() {
				ports[portName] = network.ReservePort()
			}
			network.PortsByOrdererID[orderer2.ID()] = ports
			network.Orderers = append(network.Orderers, orderer2)
			network.GenerateOrdererConfig(orderer2)
			extendNetwork(network)

			secondOrdererCertificatePath := filepath.Join(network.OrdererLocalTLSDir(orderer2), "server.crt")
			secondOrdererCertificate, err := ioutil.ReadFile(secondOrdererCertificatePath)
			Expect(err).NotTo(HaveOccurred())

			By("14) Adding the second orderer to system channel")
			addConsenter(network, peer, orderer, syschannel, protosraft.Consenter{
				ServerTlsCert: secondOrdererCertificate,
				ClientTlsCert: secondOrdererCertificate,
				Host:          "127.0.0.1",
				Port:          uint32(network.OrdererPort(orderer2, nwo.ClusterPort)),
			})

			By("15) Obtaining the last config block from the orderer")
			configBlock := nwo.GetConfigBlock(network, peer, orderer, syschannel)
			err = ioutil.WriteFile(filepath.Join(testDir, "systemchannel_block.pb"), protoutil.MarshalOrPanic(configBlock), 0o644)
			Expect(err).NotTo(HaveOccurred())

			By("16) Waiting for the existing orderer to relinquish its leadership")
			Eventually(o1Runner.Err(), network.EventuallyTimeout).Should(gbytes.Say("1 stepped down to follower since quorum is not active"))
			Eventually(o1Runner.Err(), network.EventuallyTimeout).Should(gbytes.Say("No leader is present, cluster size is 2"))

			By("17) Launching the second orderer")
			o2Runner = network.OrdererRunner(orderer2)
			o2Proc = ifrit.Invoke(o2Runner)
			Eventually(o2Proc.Ready(), network.EventuallyTimeout).Should(BeClosed())
			Eventually(o2Runner.Err(), network.EventuallyTimeout, time.Second).Should(gbytes.Say("Raft leader changed: 0 -> "))

			By("18) Adding orderer2 to channel2")
			addConsenter(network, peer, orderer, channel2, protosraft.Consenter{
				ServerTlsCert: secondOrdererCertificate,
				ClientTlsCert: secondOrdererCertificate,
				Host:          "127.0.0.1",
				Port:          uint32(network.OrdererPort(orderer2, nwo.ClusterPort)),
			})

			assertBlockReception(map[string]int{
				syschannel: int(sysBlockNum + 2),
				channel2:   int(nwo.CurrentConfigBlockNumber(network, peer, orderer, channel2)),
			}, []*nwo.Orderer{orderer2}, peer, network)

			By("19) Executing transaction against second orderer on channel2")
			assertBlockCreation(network, orderer2, nil, channel2, 3)
		})
	})
})

func validateConsensusTypeValue(value *protosorderer.ConsensusType, cType string, state protosorderer.ConsensusType_State) {
	Expect(value.Type).To(Equal(cType))
	Expect(value.State).To(Equal(state))
}

func extractOrdererConsensusType(config *common.Config) *protosorderer.ConsensusType {
	var consensusTypeValue protosorderer.ConsensusType
	consensusTypeConfigValue := config.ChannelGroup.Groups["Orderer"].Values["ConsensusType"]
	err := proto.Unmarshal(consensusTypeConfigValue.Value, &consensusTypeValue)
	Expect(err).NotTo(HaveOccurred())
	return &consensusTypeValue
}

func updateConfigWithConsensusType(
	consensusType string,
	consensusMetadata []byte,
	migState protosorderer.ConsensusType_State,
	updatedConfig *common.Config,
	consensusTypeValue *protosorderer.ConsensusType,
) {
	consensusTypeValue.Type = consensusType
	consensusTypeValue.Metadata = consensusMetadata
	consensusTypeValue.State = migState
	updatedConfig.ChannelGroup.Groups["Orderer"].Values["ConsensusType"] = &common.ConfigValue{
		ModPolicy: "Admins",
		Value:     protoutil.MarshalOrPanic(consensusTypeValue),
	}
}

func updateConfigWithBatchTimeout(updatedConfig *common.Config) {
	batchTimeoutConfigValue := updatedConfig.ChannelGroup.Groups["Orderer"].Values["BatchTimeout"]
	batchTimeoutValue := new(protosorderer.BatchTimeout)
	err := proto.Unmarshal(batchTimeoutConfigValue.Value, batchTimeoutValue)
	Expect(err).NotTo(HaveOccurred())
	toDur, err := time.ParseDuration(batchTimeoutValue.Timeout)
	Expect(err).NotTo(HaveOccurred())
	toDur = toDur + time.Duration(100000000)
	batchTimeoutValue.Timeout = toDur.String()
	By(fmt.Sprintf("Increasing BatchTimeout to %s", batchTimeoutValue.Timeout))
	updatedConfig.ChannelGroup.Groups["Orderer"].Values["BatchTimeout"] = &common.ConfigValue{
		ModPolicy: "Admins",
		Value:     protoutil.MarshalOrPanic(batchTimeoutValue),
	}
}

func solo2RaftMultiChannel() *nwo.Config {
	config := nwo.BasicSolo()
	config.Channels = []*nwo.Channel{
		{Name: "testchannel1", Profile: "TwoOrgsChannel"},
		{Name: "testchannel2", Profile: "TwoOrgsChannel"},
	}

	for _, peer := range config.Peers {
		peer.Channels = []*nwo.PeerChannel{
			{Name: "testchannel1", Anchor: true},
			{Name: "testchannel2", Anchor: true},
		}
	}
	return config
}

func prepareRaftMetadata(network *nwo.Network) []byte {
	var consenters []*protosraft.Consenter
	for _, o := range network.Orderers {
		fullTlsPath := network.OrdererLocalTLSDir(o)
		certBytes, err := ioutil.ReadFile(filepath.Join(fullTlsPath, "server.crt"))
		Expect(err).NotTo(HaveOccurred())
		port := network.OrdererPort(o, nwo.ClusterPort)

		consenter := &protosraft.Consenter{
			ClientTlsCert: certBytes,
			ServerTlsCert: certBytes,
			Host:          "127.0.0.1",
			Port:          uint32(port),
		}
		consenters = append(consenters, consenter)
	}

	raftMetadata := &protosraft.ConfigMetadata{
		Consenters: consenters,
		Options: &protosraft.Options{
			TickInterval:         "500ms",
			ElectionTick:         10,
			HeartbeatTick:        1,
			MaxInflightBlocks:    5,
			SnapshotIntervalSize: 10 * 1024 * 1024,
		},
	}

	raftMetadataBytes := protoutil.MarshalOrPanic(raftMetadata)

	return raftMetadataBytes
}

func checkPeerDeliverRequest(o *nwo.Orderer, submitter *nwo.Peer, network *nwo.Network, channelName string) error {
	c := commands.ChannelFetch{
		ChannelID:  channelName,
		Block:      "newest",
		OutputFile: "/dev/null",
		Orderer:    network.OrdererAddress(o, nwo.ListenPort),
	}

	sess, err := network.PeerUserSession(submitter, "User1", c)
	Expect(err).NotTo(HaveOccurred())
	Eventually(sess, network.EventuallyTimeout).Should(gexec.Exit())
	sessErr := string(sess.Err.Contents())
	sessExitCode := sess.ExitCode()
	if sessExitCode != 0 && strings.Contains(sessErr, "FORBIDDEN") {
		return errors.New("FORBIDDEN")
	}
	if sessExitCode == 0 && strings.Contains(sessErr, "Received block: ") {
		return nil
	}

	return fmt.Errorf("Unexpected result: ExitCode=%d, Err=%s", sessExitCode, sessErr)
}

func updateOrdererConfigFailed(n *nwo.Network, orderer *nwo.Orderer, channel string, current, updated *common.Config, peer *nwo.Peer, additionalSigners ...*nwo.Orderer) {
	sess := nwo.UpdateOrdererConfigSession(n, orderer, channel, current, updated, peer, additionalSigners...)
	Eventually(sess, n.EventuallyTimeout).Should(gexec.Exit(1))
	Expect(sess.Err).NotTo(gbytes.Say("Successfully submitted channel update"))
}

func prepareTransition(
	network *nwo.Network, peer *nwo.Peer, orderer *nwo.Orderer, channel string, // Auxiliary
	fromConsensusType string, fromMigState protosorderer.ConsensusType_State, // From
	toConsensusType string, toConsensusMetadata []byte, toMigState protosorderer.ConsensusType_State, // To
) (current, updated *common.Config) {
	current = nwo.GetConfig(network, peer, orderer, channel)
	updated = proto.Clone(current).(*common.Config)
	consensusTypeValue := extractOrdererConsensusType(current)
	validateConsensusTypeValue(consensusTypeValue, fromConsensusType, fromMigState)
	updateConfigWithConsensusType(toConsensusType, toConsensusMetadata, toMigState, updated, consensusTypeValue)
	return current, updated
}

func assertTransitionFailed(
	network *nwo.Network, peer *nwo.Peer, orderer *nwo.Orderer, channel string, // Auxiliary
	fromConsensusType string, fromMigState protosorderer.ConsensusType_State, // From
	toConsensusType string, toConsensusMetadata []byte, toMigState protosorderer.ConsensusType_State, // To
) {
	current, updated := prepareTransition(
		network, peer, orderer, channel,
		fromConsensusType, fromMigState,
		toConsensusType, toConsensusMetadata, toMigState)
	updateOrdererConfigFailed(network, orderer, channel, current, updated, peer, orderer)
}

func assertBlockCreation(network *nwo.Network, orderer *nwo.Orderer, peer *nwo.Peer, channelID string, blkNum uint64) {
	var signer *nwo.SigningIdentity
	signer = network.OrdererUserSigner(orderer, "Admin")
	if peer != nil {
		signer = network.PeerUserSigner(peer, "Admin")
	}
	env := createBroadcastEnvelope(network, signer, channelID, []byte("hola"))
	resp, err := ordererclient.Broadcast(network, orderer, env)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.Status).To(Equal(common.Status_SUCCESS))

	denv := createDeliverEnvelope(network, signer, blkNum, channelID)
	blk, err := ordererclient.Deliver(network, orderer, denv)
	Expect(err).NotTo(HaveOccurred())
	Expect(blk).ToNot(BeNil())
}

func assertTxFailed(network *nwo.Network, orderer *nwo.Orderer, channelID string) {
	signer := network.OrdererUserSigner(orderer, "Admin")
	env := createBroadcastEnvelope(network, signer, channelID, []byte("hola"))
	resp, err := ordererclient.Broadcast(network, orderer, env)
	Expect(err).NotTo(HaveOccurred())
	Expect(resp.Status).To(Equal(common.Status_SERVICE_UNAVAILABLE))
	Expect(resp.Info).To(Equal("normal transactions are rejected: maintenance mode"))
}

func createBroadcastEnvelope(n *nwo.Network, signer *nwo.SigningIdentity, channel string, data []byte) *common.Envelope {
	env, err := protoutil.CreateSignedEnvelope(
		common.HeaderType_MESSAGE,
		channel,
		signer,
		&common.Envelope{Payload: data},
		0,
		0,
	)
	Expect(err).NotTo(HaveOccurred())

	return env
}

// CreateDeliverEnvelope creates a deliver env to seek for specified block.
func createDeliverEnvelope(n *nwo.Network, signer *nwo.SigningIdentity, blkNum uint64, channel string) *common.Envelope {
	specified := &protosorderer.SeekPosition{
		Type: &protosorderer.SeekPosition_Specified{
			Specified: &protosorderer.SeekSpecified{Number: blkNum},
		},
	}
	env, err := protoutil.CreateSignedEnvelope(
		common.HeaderType_DELIVER_SEEK_INFO,
		channel,
		signer,
		&protosorderer.SeekInfo{
			Start:    specified,
			Stop:     specified,
			Behavior: protosorderer.SeekInfo_BLOCK_UNTIL_READY,
		},
		0,
		0,
	)
	Expect(err).NotTo(HaveOccurred())

	return env
}
