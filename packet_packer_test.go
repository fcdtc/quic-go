package quic

import (
	"bytes"
	"math/rand"
	"net"
	"time"

	"github.com/lucas-clemente/quic-go/internal/qerr"

	"github.com/lucas-clemente/quic-go/internal/ackhandler"

	"github.com/golang/mock/gomock"
	"github.com/lucas-clemente/quic-go/internal/handshake"
	"github.com/lucas-clemente/quic-go/internal/mocks"
	mockackhandler "github.com/lucas-clemente/quic-go/internal/mocks/ackhandler"
	"github.com/lucas-clemente/quic-go/internal/protocol"
	"github.com/lucas-clemente/quic-go/internal/wire"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/ginkgo/extensions/table"
	. "github.com/onsi/gomega"
)

var _ = Describe("Packet packer", func() {
	const maxPacketSize protocol.ByteCount = 1357
	const version = protocol.VersionTLS

	var (
		packer              *packetPacker
		retransmissionQueue *retransmissionQueue
		framer              *MockFrameSource
		ackFramer           *MockAckFrameSource
		initialStream       *MockCryptoStream
		handshakeStream     *MockCryptoStream
		sealingManager      *MockSealingManager
		pnManager           *mockackhandler.MockSentPacketHandler
	)

	checkLength := func(data []byte) {
		hdr, _, _, err := wire.ParsePacket(data, 0)
		Expect(err).ToNot(HaveOccurred())
		r := bytes.NewReader(data)
		extHdr, err := hdr.ParseExtended(r, version)
		Expect(err).ToNot(HaveOccurred())
		ExpectWithOffset(1, extHdr.Length).To(BeEquivalentTo(r.Len() + int(extHdr.PacketNumberLen)))
	}

	appendFrames := func(fs, frames []ackhandler.Frame) ([]ackhandler.Frame, protocol.ByteCount) {
		var length protocol.ByteCount
		for _, f := range frames {
			length += f.Frame.Length(packer.version)
		}
		return append(fs, frames...), length
	}

	expectAppendStreamFrames := func(frames ...ackhandler.Frame) {
		framer.EXPECT().AppendStreamFrames(gomock.Any(), gomock.Any()).DoAndReturn(func(fs []ackhandler.Frame, _ protocol.ByteCount) ([]ackhandler.Frame, protocol.ByteCount) {
			return appendFrames(fs, frames)
		})
	}

	expectAppendControlFrames := func(frames ...ackhandler.Frame) {
		framer.EXPECT().AppendControlFrames(gomock.Any(), gomock.Any()).DoAndReturn(func(fs []ackhandler.Frame, _ protocol.ByteCount) ([]ackhandler.Frame, protocol.ByteCount) {
			return appendFrames(fs, frames)
		})
	}

	BeforeEach(func() {
		rand.Seed(GinkgoRandomSeed())
		retransmissionQueue = newRetransmissionQueue(version)
		mockSender := NewMockStreamSender(mockCtrl)
		mockSender.EXPECT().onHasStreamData(gomock.Any()).AnyTimes()
		initialStream = NewMockCryptoStream(mockCtrl)
		handshakeStream = NewMockCryptoStream(mockCtrl)
		framer = NewMockFrameSource(mockCtrl)
		ackFramer = NewMockAckFrameSource(mockCtrl)
		sealingManager = NewMockSealingManager(mockCtrl)
		pnManager = mockackhandler.NewMockSentPacketHandler(mockCtrl)

		packer = newPacketPacker(
			protocol.ConnectionID{1, 2, 3, 4, 5, 6, 7, 8},
			func() protocol.ConnectionID { return protocol.ConnectionID{1, 2, 3, 4, 5, 6, 7, 8} },
			initialStream,
			handshakeStream,
			pnManager,
			retransmissionQueue,
			&net.TCPAddr{},
			sealingManager,
			framer,
			ackFramer,
			protocol.PerspectiveServer,
			version,
		)
		packer.version = version
		packer.maxPacketSize = maxPacketSize
	})

	Context("determining the maximum packet size", func() {
		It("uses the minimum initial size, if it can't determine if the remote address is IPv4 or IPv6", func() {
			Expect(getMaxPacketSize(&net.TCPAddr{})).To(BeEquivalentTo(protocol.MinInitialPacketSize))
		})

		It("uses the maximum IPv4 packet size, if the remote address is IPv4", func() {
			addr := &net.UDPAddr{IP: net.IPv4(11, 12, 13, 14), Port: 1337}
			Expect(getMaxPacketSize(addr)).To(BeEquivalentTo(protocol.MaxPacketSizeIPv4))
		})

		It("uses the maximum IPv6 packet size, if the remote address is IPv6", func() {
			ip := net.ParseIP("2001:0db8:85a3:0000:0000:8a2e:0370:7334")
			addr := &net.UDPAddr{IP: ip, Port: 1337}
			Expect(getMaxPacketSize(addr)).To(BeEquivalentTo(protocol.MaxPacketSizeIPv6))
		})
	})

	Context("generating a packet header", func() {
		It("uses the Long Header format", func() {
			pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen3)
			h := packer.getLongHeader(protocol.EncryptionHandshake)
			Expect(h.IsLongHeader).To(BeTrue())
			Expect(h.PacketNumber).To(Equal(protocol.PacketNumber(0x42)))
			Expect(h.PacketNumberLen).To(Equal(protocol.PacketNumberLen3))
			Expect(h.Version).To(Equal(packer.version))
		})

		It("sets source and destination connection ID", func() {
			pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
			srcConnID := protocol.ConnectionID{1, 2, 3, 4, 5, 6, 7, 8}
			destConnID := protocol.ConnectionID{8, 7, 6, 5, 4, 3, 2, 1}
			packer.srcConnID = srcConnID
			packer.getDestConnID = func() protocol.ConnectionID { return destConnID }
			h := packer.getLongHeader(protocol.EncryptionHandshake)
			Expect(h.SrcConnectionID).To(Equal(srcConnID))
			Expect(h.DestConnectionID).To(Equal(destConnID))
		})

		It("gets a short header", func() {
			pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x1337), protocol.PacketNumberLen4)
			h := packer.getShortHeader(protocol.KeyPhaseOne)
			Expect(h.IsLongHeader).To(BeFalse())
			Expect(h.PacketNumber).To(Equal(protocol.PacketNumber(0x1337)))
			Expect(h.PacketNumberLen).To(Equal(protocol.PacketNumberLen4))
			Expect(h.KeyPhase).To(Equal(protocol.KeyPhaseOne))
		})
	})

	Context("encrypting packets", func() {
		It("encrypts a packet", func() {
			initialStream.EXPECT().HasData()
			handshakeStream.EXPECT().HasData()
			pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x1337), protocol.PacketNumberLen2)
			pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x1337))
			sealer := mocks.NewMockShortHeaderSealer(mockCtrl)
			sealer.EXPECT().Overhead().Return(4).AnyTimes()
			var hdrRaw []byte
			gomock.InOrder(
				sealer.EXPECT().KeyPhase().Return(protocol.KeyPhaseOne),
				sealer.EXPECT().Seal(gomock.Any(), gomock.Any(), protocol.PacketNumber(0x1337), gomock.Any()).DoAndReturn(func(_, src []byte, _ protocol.PacketNumber, aad []byte) []byte {
					hdrRaw = append([]byte{}, aad...)
					return append(src, []byte{0xde, 0xca, 0xfb, 0xad}...)
				}),
				sealer.EXPECT().EncryptHeader(gomock.Any(), gomock.Any(), gomock.Any()).Do(func(sample []byte, firstByte *byte, pnBytes []byte) {
					Expect(firstByte).To(Equal(&hdrRaw[0]))
					Expect(pnBytes).To(Equal(hdrRaw[len(hdrRaw)-2:]))
					*firstByte ^= 0xff // invert the first byte
					// invert the packet number bytes
					for i := range pnBytes {
						pnBytes[i] ^= 0xff
					}
				}),
			)
			framer.EXPECT().HasData().Return(true)
			sealingManager.EXPECT().GetInitialSealer().Return(nil, nil)
			sealingManager.EXPECT().GetHandshakeSealer().Return(nil, nil)
			sealingManager.EXPECT().Get1RTTSealer().Return(sealer, nil)
			ackFramer.EXPECT().GetAckFrame(protocol.EncryptionInitial, true)
			ackFramer.EXPECT().GetAckFrame(protocol.EncryptionHandshake, true)
			ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, false)
			expectAppendControlFrames()
			f := &wire.StreamFrame{Data: []byte{0xde, 0xca, 0xfb, 0xad}}
			expectAppendStreamFrames(ackhandler.Frame{Frame: f})
			p, err := packer.PackCoalescedPacket(protocol.MaxByteCount)
			Expect(err).ToNot(HaveOccurred())
			Expect(p).ToNot(BeNil())
			Expect(p.packets).To(HaveLen(1))
			Expect(p.packets[0].frames).To(Equal([]ackhandler.Frame{{Frame: f}}))
			hdrRawEncrypted := append([]byte{}, hdrRaw...)
			hdrRawEncrypted[0] ^= 0xff
			hdrRawEncrypted[len(hdrRaw)-2] ^= 0xff
			hdrRawEncrypted[len(hdrRaw)-1] ^= 0xff
			Expect(p.buffer.Data[0:len(hdrRaw)]).To(Equal(hdrRawEncrypted))
			Expect(p.buffer.Data[p.buffer.Len()-4:]).To(Equal([]byte{0xde, 0xca, 0xfb, 0xad}))
		})
	})

	Context("packing packets", func() {
		// getSealer gets a sealer that's expected to seal exactly one packet
		getSealer := func() *mocks.MockShortHeaderSealer {
			sealer := mocks.NewMockShortHeaderSealer(mockCtrl)
			sealer.EXPECT().KeyPhase().Return(protocol.KeyPhaseOne).AnyTimes()
			sealer.EXPECT().Overhead().Return(7).AnyTimes()
			sealer.EXPECT().EncryptHeader(gomock.Any(), gomock.Any(), gomock.Any()).AnyTimes()
			sealer.EXPECT().Seal(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).DoAndReturn(func(dst, src []byte, pn protocol.PacketNumber, associatedData []byte) []byte {
				return append(src, bytes.Repeat([]byte{'s'}, sealer.Overhead())...)
			}).AnyTimes()
			return sealer
		}

		Context("packing ACK packets", func() {
			It("doesn't pack a packet if there's no ACK to send", func() {
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionInitial, true)
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionHandshake, true)
				ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, true)
				p, err := packer.MaybePackAckPacket(false)
				Expect(err).ToNot(HaveOccurred())
				Expect(p).To(BeNil())
			})

			It("packs Handshake ACK-only packets", func() {
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x42))
				sealingManager.EXPECT().GetHandshakeSealer().Return(getSealer(), nil)
				ack := &wire.AckFrame{AckRanges: []wire.AckRange{{Smallest: 1, Largest: 10}}}
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionInitial, true)
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionHandshake, true).Return(ack)
				p, err := packer.MaybePackAckPacket(false)
				Expect(err).NotTo(HaveOccurred())
				Expect(p).ToNot(BeNil())
				Expect(p.EncryptionLevel()).To(Equal(protocol.EncryptionHandshake))
				Expect(p.ack).To(Equal(ack))
			})

			It("packs 1-RTT ACK-only packets", func() {
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42))
				sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
				ack := &wire.AckFrame{AckRanges: []wire.AckRange{{Smallest: 1, Largest: 10}}}
				ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, true).Return(ack)
				p, err := packer.MaybePackAckPacket(true)
				Expect(err).NotTo(HaveOccurred())
				Expect(p).ToNot(BeNil())
				Expect(p.EncryptionLevel()).To(Equal(protocol.Encryption1RTT))
				Expect(p.ack).To(Equal(ack))
			})
		})

		Context("packing 0-RTT packets", func() {
			BeforeEach(func() {
				packer.perspective = protocol.PerspectiveClient
				sealingManager.EXPECT().GetInitialSealer().Return(nil, nil).AnyTimes()
				sealingManager.EXPECT().GetHandshakeSealer().Return(nil, nil).AnyTimes()
				sealingManager.EXPECT().Get1RTTSealer().Return(nil, handshake.ErrKeysNotYetAvailable).AnyTimes()
				initialStream.EXPECT().HasData().AnyTimes()
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionInitial, true).AnyTimes()
				handshakeStream.EXPECT().HasData().AnyTimes()
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionHandshake, true).AnyTimes()
				ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, true).AnyTimes()
			})

			It("packs a 0-RTT packet", func() {
				sealingManager.EXPECT().Get0RTTSealer().Return(getSealer(), nil).AnyTimes()
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption0RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.Encryption0RTT).Return(protocol.PacketNumber(0x42))
				cf := ackhandler.Frame{Frame: &wire.MaxDataFrame{MaximumData: 0x1337}}
				framer.EXPECT().HasData().Return(true)
				framer.EXPECT().AppendControlFrames(nil, gomock.Any()).DoAndReturn(func(frames []ackhandler.Frame, _ protocol.ByteCount) ([]ackhandler.Frame, protocol.ByteCount) {
					return append(frames, cf), cf.Length(packer.version)
				})
				// TODO: check sizes
				framer.EXPECT().AppendStreamFrames(gomock.Any(), gomock.Any()).DoAndReturn(func(frames []ackhandler.Frame, _ protocol.ByteCount) ([]ackhandler.Frame, protocol.ByteCount) {
					return frames, 0
				})
				p, err := packer.PackCoalescedPacket(protocol.MaxByteCount)
				Expect(p).ToNot(BeNil())
				Expect(err).ToNot(HaveOccurred())
				Expect(p.packets).To(HaveLen(1))
				Expect(p.packets[0].header.Type).To(Equal(protocol.PacketType0RTT))
				Expect(p.packets[0].EncryptionLevel()).To(Equal(protocol.Encryption0RTT))
				Expect(p.packets[0].frames).To(Equal([]ackhandler.Frame{cf}))
			})
		})

		Context("packing CONNECTION_CLOSE", func() {
			It("clears the reason phrase for crypto errors", func() {
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x42))
				sealingManager.EXPECT().GetInitialSealer().Return(nil, handshake.ErrKeysDropped)
				sealingManager.EXPECT().GetHandshakeSealer().Return(getSealer(), nil)
				sealingManager.EXPECT().Get1RTTSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				quicErr := qerr.NewCryptoError(0x42, "crypto error")
				quicErr.FrameType = 0x1234
				p, err := packer.PackConnectionClose(quicErr)
				Expect(err).ToNot(HaveOccurred())
				Expect(p.packets).To(HaveLen(1))
				Expect(p.packets[0].header.Type).To(Equal(protocol.PacketTypeHandshake))
				Expect(p.packets[0].frames).To(HaveLen(1))
				Expect(p.packets[0].frames[0].Frame).To(BeAssignableToTypeOf(&wire.ConnectionCloseFrame{}))
				ccf := p.packets[0].frames[0].Frame.(*wire.ConnectionCloseFrame)
				Expect(ccf.IsApplicationError).To(BeFalse())
				Expect(ccf.ErrorCode).To(BeEquivalentTo(0x100 + 0x42))
				Expect(ccf.FrameType).To(BeEquivalentTo(0x1234))
				Expect(ccf.ReasonPhrase).To(BeEmpty())
			})

			It("packs a CONNECTION_CLOSE in 1-RTT", func() {
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42))
				sealingManager.EXPECT().GetInitialSealer().Return(nil, handshake.ErrKeysDropped)
				sealingManager.EXPECT().GetHandshakeSealer().Return(nil, handshake.ErrKeysDropped)
				sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
				// expect no framer.PopStreamFrames
				p, err := packer.PackConnectionClose(qerr.NewError(qerr.CryptoBufferExceeded, "test error"))
				Expect(err).ToNot(HaveOccurred())
				Expect(p.packets).To(HaveLen(1))
				Expect(p.packets[0].header.IsLongHeader).To(BeFalse())
				Expect(p.packets[0].frames).To(HaveLen(1))
				Expect(p.packets[0].frames[0].Frame).To(BeAssignableToTypeOf(&wire.ConnectionCloseFrame{}))
				ccf := p.packets[0].frames[0].Frame.(*wire.ConnectionCloseFrame)
				Expect(ccf.IsApplicationError).To(BeFalse())
				Expect(ccf.ErrorCode).To(Equal(qerr.CryptoBufferExceeded))
				Expect(ccf.ReasonPhrase).To(Equal("test error"))
			})

			It("packs a CONNECTION_CLOSE in all available encryption levels, and replaces application errors in Initial and Handshake", func() {
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(1), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(1))
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(2), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(2))
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(3), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(3))
				sealingManager.EXPECT().GetInitialSealer().Return(getSealer(), nil)
				sealingManager.EXPECT().GetHandshakeSealer().Return(getSealer(), nil)
				sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
				p, err := packer.PackConnectionClose(qerr.NewApplicationError(0x1337, "test error"))
				Expect(err).ToNot(HaveOccurred())
				Expect(p.packets).To(HaveLen(3))
				Expect(p.packets[0].header.Type).To(Equal(protocol.PacketTypeInitial))
				Expect(p.packets[0].header.PacketNumber).To(Equal(protocol.PacketNumber(1)))
				Expect(p.packets[0].frames).To(HaveLen(1))
				Expect(p.packets[0].frames[0].Frame).To(BeAssignableToTypeOf(&wire.ConnectionCloseFrame{}))
				ccf := p.packets[0].frames[0].Frame.(*wire.ConnectionCloseFrame)
				Expect(ccf.IsApplicationError).To(BeFalse())
				Expect(ccf.ErrorCode).To(Equal(qerr.ApplicationError))
				Expect(ccf.ReasonPhrase).To(BeEmpty())
				Expect(p.packets[1].header.Type).To(Equal(protocol.PacketTypeHandshake))
				Expect(p.packets[1].header.PacketNumber).To(Equal(protocol.PacketNumber(2)))
				Expect(p.packets[1].frames).To(HaveLen(1))
				Expect(p.packets[1].frames[0].Frame).To(BeAssignableToTypeOf(&wire.ConnectionCloseFrame{}))
				ccf = p.packets[1].frames[0].Frame.(*wire.ConnectionCloseFrame)
				Expect(ccf.IsApplicationError).To(BeFalse())
				Expect(ccf.ErrorCode).To(Equal(qerr.ApplicationError))
				Expect(ccf.ReasonPhrase).To(BeEmpty())
				Expect(p.packets[2].header.IsLongHeader).To(BeFalse())
				Expect(p.packets[2].header.PacketNumber).To(Equal(protocol.PacketNumber(3)))
				Expect(p.packets[2].frames).To(HaveLen(1))
				Expect(p.packets[2].frames[0].Frame).To(BeAssignableToTypeOf(&wire.ConnectionCloseFrame{}))
				ccf = p.packets[2].frames[0].Frame.(*wire.ConnectionCloseFrame)
				Expect(ccf.IsApplicationError).To(BeTrue())
				Expect(ccf.ErrorCode).To(BeEquivalentTo(0x1337))
				Expect(ccf.ReasonPhrase).To(Equal("test error"))
			})

			It("packs a CONNECTION_CLOSE in all available encryption levels, as a client", func() {
				packer.perspective = protocol.PerspectiveClient
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(1), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(1))
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(2), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(2))
				sealingManager.EXPECT().GetInitialSealer().Return(nil, handshake.ErrKeysDropped)
				sealingManager.EXPECT().GetHandshakeSealer().Return(getSealer(), nil)
				sealingManager.EXPECT().Get0RTTSealer().Return(nil, handshake.ErrKeysDropped)
				sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
				p, err := packer.PackConnectionClose(qerr.NewApplicationError(0x1337, "test error"))
				Expect(err).ToNot(HaveOccurred())
				Expect(p.packets).To(HaveLen(2))
				Expect(p.buffer.Len()).To(BeNumerically("<", protocol.MinInitialPacketSize))
				Expect(p.packets[0].header.Type).To(Equal(protocol.PacketTypeHandshake))
				Expect(p.packets[0].header.PacketNumber).To(Equal(protocol.PacketNumber(1)))
				Expect(p.packets[0].frames).To(HaveLen(1))
				Expect(p.packets[0].frames[0].Frame).To(BeAssignableToTypeOf(&wire.ConnectionCloseFrame{}))
				ccf := p.packets[0].frames[0].Frame.(*wire.ConnectionCloseFrame)
				Expect(ccf.IsApplicationError).To(BeFalse())
				Expect(ccf.ErrorCode).To(Equal(qerr.ApplicationError))
				Expect(ccf.ReasonPhrase).To(BeEmpty())
				Expect(p.packets[1].header.IsLongHeader).To(BeFalse())
				Expect(p.packets[1].header.PacketNumber).To(Equal(protocol.PacketNumber(2)))
				Expect(p.packets[1].frames).To(HaveLen(1))
				Expect(p.packets[1].frames[0].Frame).To(BeAssignableToTypeOf(&wire.ConnectionCloseFrame{}))
				ccf = p.packets[1].frames[0].Frame.(*wire.ConnectionCloseFrame)
				Expect(ccf.IsApplicationError).To(BeTrue())
				Expect(ccf.ErrorCode).To(BeEquivalentTo(0x1337))
				Expect(ccf.ReasonPhrase).To(Equal("test error"))
			})

			It("packs a CONNECTION_CLOSE in all available encryption levels and pads, as a client", func() {
				packer.perspective = protocol.PerspectiveClient
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(1), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(1))
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption0RTT).Return(protocol.PacketNumber(2), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.Encryption0RTT).Return(protocol.PacketNumber(2))
				sealingManager.EXPECT().GetInitialSealer().Return(getSealer(), nil)
				sealingManager.EXPECT().GetHandshakeSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				sealingManager.EXPECT().Get0RTTSealer().Return(getSealer(), nil)
				sealingManager.EXPECT().Get1RTTSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				p, err := packer.PackConnectionClose(qerr.NewApplicationError(0x1337, "test error"))
				Expect(err).ToNot(HaveOccurred())
				Expect(p.packets).To(HaveLen(2))
				Expect(p.buffer.Len()).To(BeNumerically(">=", protocol.MinInitialPacketSize))
				Expect(p.buffer.Len()).To(BeEquivalentTo(maxPacketSize))
				Expect(p.packets[0].header.Type).To(Equal(protocol.PacketTypeInitial))
				Expect(p.packets[0].header.PacketNumber).To(Equal(protocol.PacketNumber(1)))
				Expect(p.packets[0].frames).To(HaveLen(1))
				Expect(p.packets[0].frames[0].Frame).To(BeAssignableToTypeOf(&wire.ConnectionCloseFrame{}))
				ccf := p.packets[0].frames[0].Frame.(*wire.ConnectionCloseFrame)
				Expect(ccf.IsApplicationError).To(BeFalse())
				Expect(ccf.ErrorCode).To(Equal(qerr.ApplicationError))
				Expect(ccf.ReasonPhrase).To(BeEmpty())
				Expect(p.packets[1].header.Type).To(Equal(protocol.PacketType0RTT))
				Expect(p.packets[1].header.PacketNumber).To(Equal(protocol.PacketNumber(2)))
				Expect(p.packets[1].frames).To(HaveLen(1))
				Expect(p.packets[1].frames[0].Frame).To(BeAssignableToTypeOf(&wire.ConnectionCloseFrame{}))
				ccf = p.packets[1].frames[0].Frame.(*wire.ConnectionCloseFrame)
				Expect(ccf.IsApplicationError).To(BeTrue())
				Expect(ccf.ErrorCode).To(BeEquivalentTo(0x1337))
				Expect(ccf.ReasonPhrase).To(Equal("test error"))
			})
		})

		Context("packing normal packets", func() {
			It("returns nil when no packet is queued", func() {
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				// don't expect any calls to PopPacketNumber
				sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
				ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, true)
				framer.EXPECT().HasData()
				p, err := packer.PackPacket()
				Expect(p).To(BeNil())
				Expect(err).ToNot(HaveOccurred())
			})

			It("packs single packets", func() {
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42))
				sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
				framer.EXPECT().HasData().Return(true)
				ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, false)
				expectAppendControlFrames()
				f := &wire.StreamFrame{
					StreamID: 5,
					Data:     []byte{0xde, 0xca, 0xfb, 0xad},
				}
				expectAppendStreamFrames(ackhandler.Frame{Frame: f})
				p, err := packer.PackPacket()
				Expect(err).ToNot(HaveOccurred())
				Expect(p).ToNot(BeNil())
				b := &bytes.Buffer{}
				f.Write(b, packer.version)
				Expect(p.frames).To(Equal([]ackhandler.Frame{{Frame: f}}))
				Expect(p.buffer.Data).To(ContainSubstring(b.String()))
			})

			It("stores the encryption level a packet was sealed with", func() {
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42))
				sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
				framer.EXPECT().HasData().Return(true)
				ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, false)
				expectAppendControlFrames()
				expectAppendStreamFrames(ackhandler.Frame{Frame: &wire.StreamFrame{
					StreamID: 5,
					Data:     []byte("foobar"),
				}})
				p, err := packer.PackPacket()
				Expect(err).ToNot(HaveOccurred())
				Expect(p.EncryptionLevel()).To(Equal(protocol.Encryption1RTT))
			})

			It("packs a single ACK", func() {
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42))
				ack := &wire.AckFrame{AckRanges: []wire.AckRange{{Largest: 42, Smallest: 1}}}
				framer.EXPECT().HasData()
				ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, true).Return(ack)
				sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
				p, err := packer.PackPacket()
				Expect(err).NotTo(HaveOccurred())
				Expect(p).ToNot(BeNil())
				Expect(p.ack).To(Equal(ack))
			})

			It("packs control frames", func() {
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42))
				sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
				framer.EXPECT().HasData().Return(true)
				ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, false)
				frames := []ackhandler.Frame{
					{Frame: &wire.ResetStreamFrame{}},
					{Frame: &wire.MaxDataFrame{}},
				}
				expectAppendControlFrames(frames...)
				expectAppendStreamFrames()
				p, err := packer.PackPacket()
				Expect(p).ToNot(BeNil())
				Expect(err).ToNot(HaveOccurred())
				Expect(p.frames).To(Equal(frames))
				Expect(p.buffer.Len()).ToNot(BeZero())
			})

			It("accounts for the space consumed by control frames", func() {
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
				framer.EXPECT().HasData().Return(true)
				ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, false)
				var maxSize protocol.ByteCount
				gomock.InOrder(
					framer.EXPECT().AppendControlFrames(gomock.Any(), gomock.Any()).DoAndReturn(func(fs []ackhandler.Frame, maxLen protocol.ByteCount) ([]ackhandler.Frame, protocol.ByteCount) {
						maxSize = maxLen
						return fs, 444
					}),
					framer.EXPECT().AppendStreamFrames(gomock.Any(), gomock.Any()).Do(func(fs []ackhandler.Frame, maxLen protocol.ByteCount) ([]ackhandler.Frame, protocol.ByteCount) {
						Expect(maxLen).To(Equal(maxSize - 444))
						return fs, 0
					}),
				)
				_, err := packer.PackPacket()
				Expect(err).ToNot(HaveOccurred())
			})

			It("pads if payload length + packet number length is smaller than 4, for Long Header packets", func() {
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen1)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x42))
				sealer := getSealer()
				sealingManager.EXPECT().GetInitialSealer().Return(nil, handshake.ErrKeysDropped)
				sealingManager.EXPECT().GetHandshakeSealer().Return(sealer, nil)
				sealingManager.EXPECT().Get1RTTSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				packer.retransmissionQueue.AddHandshake(&wire.PingFrame{})
				handshakeStream.EXPECT().HasData()
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionHandshake, false)
				packet, err := packer.PackCoalescedPacket(protocol.MaxByteCount)
				Expect(err).ToNot(HaveOccurred())
				Expect(packet).ToNot(BeNil())
				Expect(packet.packets).To(HaveLen(1))
				// cut off the tag that the mock sealer added
				// packet.buffer.Data = packet.buffer.Data[:packet.buffer.Len()-protocol.ByteCount(sealer.Overhead())]
				hdr, _, _, err := wire.ParsePacket(packet.buffer.Data, len(packer.getDestConnID()))
				Expect(err).ToNot(HaveOccurred())
				r := bytes.NewReader(packet.buffer.Data)
				extHdr, err := hdr.ParseExtended(r, packer.version)
				Expect(err).ToNot(HaveOccurred())
				Expect(extHdr.PacketNumberLen).To(Equal(protocol.PacketNumberLen1))
				Expect(r.Len()).To(Equal(4 - 1 /* packet number length */ + sealer.Overhead()))
				// the first bytes of the payload should be a 2 PADDING frames...
				firstPayloadByte, err := r.ReadByte()
				Expect(err).ToNot(HaveOccurred())
				Expect(firstPayloadByte).To(Equal(byte(0)))
				secondPayloadByte, err := r.ReadByte()
				Expect(err).ToNot(HaveOccurred())
				Expect(secondPayloadByte).To(Equal(byte(0)))
				// ... followed by the PING
				frameParser := wire.NewFrameParser(packer.version)
				frame, err := frameParser.ParseNext(r, protocol.Encryption1RTT)
				Expect(err).ToNot(HaveOccurred())
				Expect(frame).To(BeAssignableToTypeOf(&wire.PingFrame{}))
				Expect(r.Len()).To(Equal(sealer.Overhead()))
			})

			It("pads if payload length + packet number length is smaller than 4", func() {
				f := &wire.StreamFrame{
					StreamID: 0x10, // small stream ID, such that only a single byte is consumed
					Fin:      true,
				}
				Expect(f.Length(packer.version)).To(BeEquivalentTo(2))
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen1)
				pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42))
				sealer := getSealer()
				sealingManager.EXPECT().Get1RTTSealer().Return(sealer, nil)
				framer.EXPECT().HasData().Return(true)
				ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, false)
				expectAppendControlFrames()
				expectAppendStreamFrames(ackhandler.Frame{Frame: f})
				packet, err := packer.PackPacket()
				Expect(err).ToNot(HaveOccurred())
				// cut off the tag that the mock sealer added
				packet.buffer.Data = packet.buffer.Data[:packet.buffer.Len()-protocol.ByteCount(sealer.Overhead())]
				hdr, _, _, err := wire.ParsePacket(packet.buffer.Data, len(packer.getDestConnID()))
				Expect(err).ToNot(HaveOccurred())
				r := bytes.NewReader(packet.buffer.Data)
				extHdr, err := hdr.ParseExtended(r, packer.version)
				Expect(err).ToNot(HaveOccurred())
				Expect(extHdr.PacketNumberLen).To(Equal(protocol.PacketNumberLen1))
				Expect(r.Len()).To(Equal(4 - 1 /* packet number length */))
				// the first byte of the payload should be a PADDING frame...
				firstPayloadByte, err := r.ReadByte()
				Expect(err).ToNot(HaveOccurred())
				Expect(firstPayloadByte).To(Equal(byte(0)))
				// ... followed by the STREAM frame
				frameParser := wire.NewFrameParser(packer.version)
				frame, err := frameParser.ParseNext(r, protocol.Encryption1RTT)
				Expect(err).ToNot(HaveOccurred())
				Expect(frame).To(BeAssignableToTypeOf(&wire.StreamFrame{}))
				sf := frame.(*wire.StreamFrame)
				Expect(sf.StreamID).To(Equal(f.StreamID))
				Expect(sf.Fin).To(Equal(f.Fin))
				Expect(sf.Data).To(BeEmpty())
				Expect(r.Len()).To(BeZero())
			})

			It("packs multiple small STREAM frames into single packet", func() {
				f1 := &wire.StreamFrame{
					StreamID:       5,
					Data:           []byte("frame 1"),
					DataLenPresent: true,
				}
				f2 := &wire.StreamFrame{
					StreamID:       5,
					Data:           []byte("frame 2"),
					DataLenPresent: true,
				}
				f3 := &wire.StreamFrame{
					StreamID:       3,
					Data:           []byte("frame 3"),
					DataLenPresent: true,
				}
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42))
				sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
				framer.EXPECT().HasData().Return(true)
				ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, false)
				expectAppendControlFrames()
				expectAppendStreamFrames(ackhandler.Frame{Frame: f1}, ackhandler.Frame{Frame: f2}, ackhandler.Frame{Frame: f3})
				p, err := packer.PackPacket()
				Expect(p).ToNot(BeNil())
				Expect(err).ToNot(HaveOccurred())
				Expect(p.frames).To(HaveLen(3))
				Expect(p.frames[0].Frame.(*wire.StreamFrame).Data).To(Equal([]byte("frame 1")))
				Expect(p.frames[1].Frame.(*wire.StreamFrame).Data).To(Equal([]byte("frame 2")))
				Expect(p.frames[2].Frame.(*wire.StreamFrame).Data).To(Equal([]byte("frame 3")))
			})

			Context("making ACK packets ack-eliciting", func() {
				sendMaxNumNonAckElicitingAcks := func() {
					for i := 0; i < protocol.MaxNonAckElicitingAcks; i++ {
						pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
						pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42))
						sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
						framer.EXPECT().HasData().Return(true)
						ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, false).Return(&wire.AckFrame{AckRanges: []wire.AckRange{{Smallest: 1, Largest: 1}}})
						expectAppendControlFrames()
						expectAppendStreamFrames()
						p, err := packer.PackPacket()
						Expect(p).ToNot(BeNil())
						Expect(err).ToNot(HaveOccurred())
						Expect(p.ack).ToNot(BeNil())
						Expect(p.frames).To(BeEmpty())
					}
				}

				It("adds a PING frame when it's supposed to send a ack-eliciting packet", func() {
					sendMaxNumNonAckElicitingAcks()
					pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
					pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42))
					sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
					framer.EXPECT().HasData().Return(true)
					ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, false).Return(&wire.AckFrame{AckRanges: []wire.AckRange{{Smallest: 1, Largest: 1}}})
					expectAppendControlFrames()
					expectAppendStreamFrames()
					p, err := packer.PackPacket()
					Expect(p).ToNot(BeNil())
					Expect(err).ToNot(HaveOccurred())
					Expect(p.frames).To(ContainElement(ackhandler.Frame{Frame: &wire.PingFrame{}}))
					// make sure the next packet doesn't contain another PING
					pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
					pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42))
					sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
					framer.EXPECT().HasData().Return(true)
					ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, false).Return(&wire.AckFrame{AckRanges: []wire.AckRange{{Smallest: 1, Largest: 1}}})
					expectAppendControlFrames()
					expectAppendStreamFrames()
					p, err = packer.PackPacket()
					Expect(p).ToNot(BeNil())
					Expect(err).ToNot(HaveOccurred())
					Expect(p.ack).ToNot(BeNil())
					Expect(p.frames).To(BeEmpty())
				})

				It("waits until there's something to send before adding a PING frame", func() {
					sendMaxNumNonAckElicitingAcks()
					// nothing to send
					pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
					sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
					framer.EXPECT().HasData().Return(true)
					expectAppendControlFrames()
					expectAppendStreamFrames()
					ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, false)
					p, err := packer.PackPacket()
					Expect(err).ToNot(HaveOccurred())
					Expect(p).To(BeNil())
					// now add some frame to send
					expectAppendControlFrames()
					expectAppendStreamFrames()
					pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
					pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42))
					sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
					framer.EXPECT().HasData().Return(true)
					ack := &wire.AckFrame{AckRanges: []wire.AckRange{{Smallest: 1, Largest: 1}}}
					ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, false).Return(ack)
					p, err = packer.PackPacket()
					Expect(err).ToNot(HaveOccurred())
					Expect(p.ack).To(Equal(ack))
					Expect(p.frames).To(Equal([]ackhandler.Frame{{Frame: &wire.PingFrame{}}}))
				})

				It("doesn't send a PING if it already sent another ack-eliciting frame", func() {
					sendMaxNumNonAckElicitingAcks()
					pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
					pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42))
					sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
					framer.EXPECT().HasData().Return(true)
					ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, false)
					expectAppendStreamFrames()
					expectAppendControlFrames(ackhandler.Frame{Frame: &wire.MaxDataFrame{}})
					p, err := packer.PackPacket()
					Expect(err).ToNot(HaveOccurred())
					Expect(p).ToNot(BeNil())
					Expect(p.frames).ToNot(ContainElement(&wire.PingFrame{}))
				})
			})

			Context("max packet size", func() {
				It("sets the maximum packet size", func() {
					pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2).Times(2)
					sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil).Times(2)
					framer.EXPECT().HasData().Return(true).Times(2)
					ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, false).Times(2)
					var initialMaxPacketSize protocol.ByteCount
					framer.EXPECT().AppendControlFrames(gomock.Any(), gomock.Any()).Do(func(_ []ackhandler.Frame, maxLen protocol.ByteCount) ([]ackhandler.Frame, protocol.ByteCount) {
						initialMaxPacketSize = maxLen
						return nil, 0
					})
					expectAppendStreamFrames()
					_, err := packer.PackPacket()
					Expect(err).ToNot(HaveOccurred())
					// now reduce the maxPacketSize
					packer.HandleTransportParameters(&wire.TransportParameters{
						MaxUDPPayloadSize: maxPacketSize - 10,
					})
					framer.EXPECT().AppendControlFrames(gomock.Any(), gomock.Any()).Do(func(_ []ackhandler.Frame, maxLen protocol.ByteCount) ([]ackhandler.Frame, protocol.ByteCount) {
						Expect(maxLen).To(Equal(initialMaxPacketSize - 10))
						return nil, 0
					})
					expectAppendStreamFrames()
					_, err = packer.PackPacket()
					Expect(err).ToNot(HaveOccurred())
				})

				It("doesn't increase the max packet size", func() {
					pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2).Times(2)
					sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil).Times(2)
					framer.EXPECT().HasData().Return(true).Times(2)
					ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, false).Times(2)
					var initialMaxPacketSize protocol.ByteCount
					framer.EXPECT().AppendControlFrames(gomock.Any(), gomock.Any()).Do(func(_ []ackhandler.Frame, maxLen protocol.ByteCount) ([]ackhandler.Frame, protocol.ByteCount) {
						initialMaxPacketSize = maxLen
						return nil, 0
					})
					expectAppendStreamFrames()
					_, err := packer.PackPacket()
					Expect(err).ToNot(HaveOccurred())
					// now try to increase the maxPacketSize
					packer.HandleTransportParameters(&wire.TransportParameters{
						MaxUDPPayloadSize: maxPacketSize + 10,
					})
					framer.EXPECT().AppendControlFrames(gomock.Any(), gomock.Any()).Do(func(_ []ackhandler.Frame, maxLen protocol.ByteCount) ([]ackhandler.Frame, protocol.ByteCount) {
						Expect(maxLen).To(Equal(initialMaxPacketSize))
						return nil, 0
					})
					expectAppendStreamFrames()
					_, err = packer.PackPacket()
					Expect(err).ToNot(HaveOccurred())
				})
			})
		})

		Context("packing crypto packets", func() {
			It("sets the length", func() {
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x42))
				f := &wire.CryptoFrame{
					Offset: 0x1337,
					Data:   []byte("foobar"),
				}
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionInitial, false)
				initialStream.EXPECT().HasData().Return(true).AnyTimes()
				initialStream.EXPECT().PopCryptoFrame(gomock.Any()).Return(f)
				sealingManager.EXPECT().GetInitialSealer().Return(getSealer(), nil)
				sealingManager.EXPECT().GetHandshakeSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				sealingManager.EXPECT().Get1RTTSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				p, err := packer.PackCoalescedPacket(protocol.MaxByteCount)
				Expect(err).ToNot(HaveOccurred())
				checkLength(p.buffer.Data)
			})

			It("packs a maximum size Handshake packet", func() {
				var f *wire.CryptoFrame
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x42))
				sealingManager.EXPECT().GetInitialSealer().Return(mocks.NewMockShortHeaderSealer(mockCtrl), nil)
				sealingManager.EXPECT().GetHandshakeSealer().Return(getSealer(), nil)
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionInitial, true)
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionHandshake, false)
				initialStream.EXPECT().HasData()
				handshakeStream.EXPECT().HasData().Return(true).Times(2)
				handshakeStream.EXPECT().PopCryptoFrame(gomock.Any()).DoAndReturn(func(size protocol.ByteCount) *wire.CryptoFrame {
					f = &wire.CryptoFrame{Offset: 0x1337}
					f.Data = bytes.Repeat([]byte{'f'}, int(size-f.Length(packer.version)-1))
					Expect(f.Length(packer.version)).To(Equal(size))
					return f
				})
				p, err := packer.PackCoalescedPacket(protocol.MaxByteCount)
				Expect(err).ToNot(HaveOccurred())
				Expect(p.packets).To(HaveLen(1))
				Expect(p.packets[0].frames).To(HaveLen(1))
				Expect(p.packets[0].header.IsLongHeader).To(BeTrue())
				Expect(p.buffer.Len()).To(BeEquivalentTo(packer.maxPacketSize))
				checkLength(p.buffer.Data)
			})

			It("packs a coalesced packet with Initial / Handshake", func() {
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x24), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x24))
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x42))
				sealingManager.EXPECT().GetInitialSealer().Return(getSealer(), nil)
				sealingManager.EXPECT().GetHandshakeSealer().Return(getSealer(), nil)
				sealingManager.EXPECT().Get1RTTSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionInitial, false)
				// don't EXPECT any calls for a Handshake ACK frame
				initialStream.EXPECT().HasData().Return(true).Times(2)
				initialStream.EXPECT().PopCryptoFrame(gomock.Any()).DoAndReturn(func(size protocol.ByteCount) *wire.CryptoFrame {
					return &wire.CryptoFrame{Offset: 0x42, Data: []byte("initial")}
				})
				handshakeStream.EXPECT().HasData().Return(true).Times(2)
				handshakeStream.EXPECT().PopCryptoFrame(gomock.Any()).DoAndReturn(func(size protocol.ByteCount) *wire.CryptoFrame {
					return &wire.CryptoFrame{Offset: 0x1337, Data: []byte("handshake")}
				})
				p, err := packer.PackCoalescedPacket(protocol.MaxByteCount)
				Expect(err).ToNot(HaveOccurred())
				Expect(p.packets).To(HaveLen(2))
				Expect(p.packets[0].EncryptionLevel()).To(Equal(protocol.EncryptionInitial))
				Expect(p.packets[0].frames).To(HaveLen(1))
				Expect(p.packets[0].frames[0].Frame.(*wire.CryptoFrame).Data).To(Equal([]byte("initial")))
				Expect(p.packets[1].EncryptionLevel()).To(Equal(protocol.EncryptionHandshake))
				Expect(p.packets[1].frames).To(HaveLen(1))
				Expect(p.packets[1].frames[0].Frame.(*wire.CryptoFrame).Data).To(Equal([]byte("handshake")))
				hdr, _, rest, err := wire.ParsePacket(p.buffer.Data, 0)
				Expect(err).ToNot(HaveOccurred())
				Expect(hdr.Type).To(Equal(protocol.PacketTypeInitial))
				hdr, _, rest, err = wire.ParsePacket(rest, 0)
				Expect(err).ToNot(HaveOccurred())
				Expect(hdr.Type).To(Equal(protocol.PacketTypeHandshake))
				Expect(rest).To(BeEmpty())
			})

			It("packs a coalesced packet with Initial / 0-RTT, and pads it", func() {
				packer.perspective = protocol.PerspectiveClient
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x24), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x24))
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption0RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.Encryption0RTT).Return(protocol.PacketNumber(0x42))
				sealingManager.EXPECT().GetInitialSealer().Return(getSealer(), nil)
				sealingManager.EXPECT().GetHandshakeSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				sealingManager.EXPECT().Get0RTTSealer().Return(getSealer(), nil)
				sealingManager.EXPECT().Get1RTTSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				framer.EXPECT().HasData().Return(true)
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionInitial, false)
				// don't EXPECT any calls for a Handshake ACK frame
				initialStream.EXPECT().HasData().Return(true).Times(2)
				initialStream.EXPECT().PopCryptoFrame(gomock.Any()).DoAndReturn(func(size protocol.ByteCount) *wire.CryptoFrame {
					return &wire.CryptoFrame{Offset: 0x42, Data: []byte("initial")}
				})
				expectAppendControlFrames()
				expectAppendStreamFrames(ackhandler.Frame{Frame: &wire.StreamFrame{Data: []byte("foobar")}})
				p, err := packer.PackCoalescedPacket(protocol.MaxByteCount)
				Expect(err).ToNot(HaveOccurred())
				Expect(p.buffer.Len()).To(BeNumerically(">=", protocol.MinInitialPacketSize))
				Expect(p.buffer.Len()).To(BeEquivalentTo(maxPacketSize))
				Expect(p.packets).To(HaveLen(2))
				Expect(p.packets[0].EncryptionLevel()).To(Equal(protocol.EncryptionInitial))
				Expect(p.packets[0].frames).To(HaveLen(1))
				Expect(p.packets[0].frames[0].Frame.(*wire.CryptoFrame).Data).To(Equal([]byte("initial")))
				Expect(p.packets[1].EncryptionLevel()).To(Equal(protocol.Encryption0RTT))
				Expect(p.packets[1].frames).To(HaveLen(1))
				Expect(p.packets[1].frames[0].Frame.(*wire.StreamFrame).Data).To(Equal([]byte("foobar")))
				hdr, _, rest, err := wire.ParsePacket(p.buffer.Data, 0)
				Expect(err).ToNot(HaveOccurred())
				Expect(hdr.Type).To(Equal(protocol.PacketTypeInitial))
				hdr, _, rest, err = wire.ParsePacket(rest, 0)
				Expect(err).ToNot(HaveOccurred())
				Expect(hdr.Type).To(Equal(protocol.PacketType0RTT))
				Expect(rest).To(Equal(make([]byte, len(rest))))
			})

			It("packs a coalesced packet with Handshake / 1-RTT", func() {
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x24), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x24))
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42))
				sealingManager.EXPECT().GetInitialSealer().Return(nil, handshake.ErrKeysDropped)
				sealingManager.EXPECT().GetHandshakeSealer().Return(getSealer(), nil)
				sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
				framer.EXPECT().HasData().Return(true)
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionHandshake, false)
				// don't EXPECT any calls for a 1-RTT ACK frame
				handshakeStream.EXPECT().HasData().Return(true).Times(2)
				handshakeStream.EXPECT().PopCryptoFrame(gomock.Any()).DoAndReturn(func(size protocol.ByteCount) *wire.CryptoFrame {
					return &wire.CryptoFrame{Offset: 0x1337, Data: []byte("handshake")}
				})
				expectAppendControlFrames()
				expectAppendStreamFrames(ackhandler.Frame{Frame: &wire.StreamFrame{Data: []byte("foobar")}})
				p, err := packer.PackCoalescedPacket(protocol.MaxByteCount)
				Expect(err).ToNot(HaveOccurred())
				Expect(p.packets).To(HaveLen(2))
				Expect(p.packets[0].EncryptionLevel()).To(Equal(protocol.EncryptionHandshake))
				Expect(p.packets[0].frames).To(HaveLen(1))
				Expect(p.packets[0].frames[0].Frame.(*wire.CryptoFrame).Data).To(Equal([]byte("handshake")))
				Expect(p.packets[1].EncryptionLevel()).To(Equal(protocol.Encryption1RTT))
				Expect(p.packets[1].frames).To(HaveLen(1))
				Expect(p.packets[1].frames[0].Frame.(*wire.StreamFrame).Data).To(Equal([]byte("foobar")))
				hdr, _, rest, err := wire.ParsePacket(p.buffer.Data, 0)
				Expect(err).ToNot(HaveOccurred())
				Expect(hdr.Type).To(Equal(protocol.PacketTypeHandshake))
				hdr, _, rest, err = wire.ParsePacket(rest, 0)
				Expect(err).ToNot(HaveOccurred())
				Expect(hdr.IsLongHeader).To(BeFalse())
				Expect(rest).To(BeEmpty())
			})

			It("doesn't add a coalesced packet if the remaining size is smaller than MaxCoalescedPacketSize", func() {
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x24), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x24))
				sealingManager.EXPECT().GetInitialSealer().Return(getSealer(), nil)
				// don't EXPECT any calls to GetHandshakeSealer and Get1RTTSealer
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionInitial, false)
				initialStream.EXPECT().HasData().Return(true).Times(2)
				initialStream.EXPECT().PopCryptoFrame(gomock.Any()).DoAndReturn(func(size protocol.ByteCount) *wire.CryptoFrame {
					s := size - protocol.MinCoalescedPacketSize
					f := &wire.CryptoFrame{Offset: 0x1337}
					f.Data = bytes.Repeat([]byte{'f'}, int(s-f.Length(packer.version)-1))
					Expect(f.Length(packer.version)).To(Equal(s))
					return f
				})
				p, err := packer.PackCoalescedPacket(protocol.MaxByteCount)
				Expect(err).ToNot(HaveOccurred())
				Expect(p.packets).To(HaveLen(1))
				Expect(p.packets[0].EncryptionLevel()).To(Equal(protocol.EncryptionInitial))
				Expect(len(p.buffer.Data)).To(BeEquivalentTo(maxPacketSize - protocol.MinCoalescedPacketSize))
				checkLength(p.buffer.Data)
			})

			It("doesn't pack a coalesced packet if there's not enough space", func() {
				p, err := packer.PackCoalescedPacket(protocol.MinCoalescedPacketSize - 1)
				Expect(err).ToNot(HaveOccurred())
				Expect(p).To(BeNil())
			})

			It("packs a small packet", func() {
				const size = protocol.MinCoalescedPacketSize + 10
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x24), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x24))
				sealingManager.EXPECT().GetInitialSealer().Return(getSealer(), nil)
				// don't EXPECT any calls to GetHandshakeSealer and Get1RTTSealer
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionInitial, false)
				initialStream.EXPECT().HasData().Return(true).Times(2)
				initialStream.EXPECT().PopCryptoFrame(gomock.Any()).DoAndReturn(func(s protocol.ByteCount) *wire.CryptoFrame {
					f := &wire.CryptoFrame{Offset: 0x1337}
					f.Data = bytes.Repeat([]byte{'f'}, int(s-f.Length(packer.version)-1))
					Expect(f.Length(packer.version)).To(Equal(s))
					return f
				})
				p, err := packer.PackCoalescedPacket(size)
				Expect(err).ToNot(HaveOccurred())
				Expect(p).ToNot(BeNil())
				Expect(len(p.buffer.Data)).To(Equal(size))
			})

			It("packs a small packet, that includes a 1-RTT packet", func() {
				const size = 2 * protocol.MinCoalescedPacketSize
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x24), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x24))
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x24), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x24))
				sealingManager.EXPECT().GetInitialSealer().Return(nil, handshake.ErrKeysDropped)
				sealingManager.EXPECT().GetHandshakeSealer().Return(getSealer(), nil)
				oneRTTSealer := getSealer()
				sealingManager.EXPECT().Get1RTTSealer().Return(oneRTTSealer, nil)
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionHandshake, false)
				framer.EXPECT().HasData().Return(true)
				handshakeStream.EXPECT().HasData().Return(true).Times(2)
				handshakeStream.EXPECT().PopCryptoFrame(gomock.Any()).Return(&wire.CryptoFrame{
					Offset: 0x1337,
					Data:   []byte("foobar"),
				})
				expectAppendControlFrames()
				var appDataSize protocol.ByteCount
				framer.EXPECT().AppendStreamFrames(gomock.Any(), gomock.Any()).DoAndReturn(func(frames []ackhandler.Frame, maxSize protocol.ByteCount) ([]ackhandler.Frame, protocol.ByteCount) {
					appDataSize = maxSize
					f := &wire.StreamFrame{Data: []byte("foobar")}
					return append(frames, ackhandler.Frame{Frame: f}), f.Length(packer.version)
				})
				p, err := packer.PackCoalescedPacket(size)
				Expect(err).ToNot(HaveOccurred())
				Expect(p).ToNot(BeNil())
				Expect(p.packets).To(HaveLen(2))
				Expect(appDataSize).To(Equal(size - p.packets[0].length - p.packets[1].header.GetLength(packer.version) - protocol.ByteCount(oneRTTSealer.Overhead())))
			})

			It("pads if payload length + packet number length is smaller than 4, for Long Header packets", func() {
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen1)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x42))
				sealer := getSealer()
				sealingManager.EXPECT().GetInitialSealer().Return(nil, handshake.ErrKeysDropped)
				sealingManager.EXPECT().GetHandshakeSealer().Return(sealer, nil)
				sealingManager.EXPECT().Get1RTTSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				packer.retransmissionQueue.AddHandshake(&wire.PingFrame{})
				handshakeStream.EXPECT().HasData()
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionHandshake, false)
				packet, err := packer.PackCoalescedPacket(protocol.MaxByteCount)
				Expect(err).ToNot(HaveOccurred())
				Expect(packet).ToNot(BeNil())
				Expect(packet.packets).To(HaveLen(1))
				// cut off the tag that the mock sealer added
				// packet.buffer.Data = packet.buffer.Data[:packet.buffer.Len()-protocol.ByteCount(sealer.Overhead())]
				hdr, _, _, err := wire.ParsePacket(packet.buffer.Data, len(packer.getDestConnID()))
				Expect(err).ToNot(HaveOccurred())
				r := bytes.NewReader(packet.buffer.Data)
				extHdr, err := hdr.ParseExtended(r, packer.version)
				Expect(err).ToNot(HaveOccurred())
				Expect(extHdr.PacketNumberLen).To(Equal(protocol.PacketNumberLen1))
				Expect(r.Len()).To(Equal(4 - 1 /* packet number length */ + sealer.Overhead()))
				// the first bytes of the payload should be a 2 PADDING frames...
				firstPayloadByte, err := r.ReadByte()
				Expect(err).ToNot(HaveOccurred())
				Expect(firstPayloadByte).To(Equal(byte(0)))
				secondPayloadByte, err := r.ReadByte()
				Expect(err).ToNot(HaveOccurred())
				Expect(secondPayloadByte).To(Equal(byte(0)))
				// ... followed by the PING
				frameParser := wire.NewFrameParser(packer.version)
				frame, err := frameParser.ParseNext(r, protocol.Encryption1RTT)
				Expect(err).ToNot(HaveOccurred())
				Expect(frame).To(BeAssignableToTypeOf(&wire.PingFrame{}))
				Expect(r.Len()).To(Equal(sealer.Overhead()))
			})

			It("adds retransmissions", func() {
				f := &wire.CryptoFrame{Data: []byte("Initial")}
				retransmissionQueue.AddInitial(f)
				retransmissionQueue.AddHandshake(&wire.CryptoFrame{Data: []byte("Handshake")})
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x42))
				sealingManager.EXPECT().GetInitialSealer().Return(getSealer(), nil)
				sealingManager.EXPECT().GetHandshakeSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				sealingManager.EXPECT().Get1RTTSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionInitial, false)
				initialStream.EXPECT().HasData()
				p, err := packer.PackCoalescedPacket(protocol.MaxByteCount)
				Expect(err).ToNot(HaveOccurred())
				Expect(p.packets).To(HaveLen(1))
				Expect(p.packets[0].EncryptionLevel()).To(Equal(protocol.EncryptionInitial))
				Expect(p.packets[0].frames).To(Equal([]ackhandler.Frame{{Frame: f}}))
				Expect(p.packets[0].header.IsLongHeader).To(BeTrue())
				checkLength(p.buffer.Data)
			})

			It("sends an Initial packet containing only an ACK", func() {
				ack := &wire.AckFrame{AckRanges: []wire.AckRange{{Smallest: 10, Largest: 20}}}
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionInitial, true).Return(ack)
				initialStream.EXPECT().HasData().Times(2)
				sealingManager.EXPECT().GetInitialSealer().Return(getSealer(), nil)
				sealingManager.EXPECT().GetHandshakeSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				sealingManager.EXPECT().Get1RTTSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x42))
				p, err := packer.PackCoalescedPacket(protocol.MaxByteCount)
				Expect(err).ToNot(HaveOccurred())
				Expect(p.packets).To(HaveLen(1))
				Expect(p.packets[0].ack).To(Equal(ack))
			})

			It("doesn't pack anything if there's nothing to send at Initial and Handshake keys are not yet available", func() {
				sealingManager.EXPECT().GetInitialSealer().Return(getSealer(), nil)
				sealingManager.EXPECT().GetHandshakeSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				sealingManager.EXPECT().Get1RTTSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				initialStream.EXPECT().HasData()
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionInitial, true)
				p, err := packer.PackCoalescedPacket(protocol.MaxByteCount)
				Expect(err).ToNot(HaveOccurred())
				Expect(p).To(BeNil())
			})

			It("sends a Handshake packet containing only an ACK", func() {
				ack := &wire.AckFrame{AckRanges: []wire.AckRange{{Smallest: 10, Largest: 20}}}
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionInitial, true)
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionHandshake, true).Return(ack)
				initialStream.EXPECT().HasData()
				handshakeStream.EXPECT().HasData().Times(2)
				sealingManager.EXPECT().GetInitialSealer().Return(mocks.NewMockShortHeaderSealer(mockCtrl), nil)
				sealingManager.EXPECT().GetHandshakeSealer().Return(getSealer(), nil)
				sealingManager.EXPECT().Get1RTTSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x42))
				p, err := packer.PackCoalescedPacket(protocol.MaxByteCount)
				Expect(err).ToNot(HaveOccurred())
				Expect(p.packets).To(HaveLen(1))
				Expect(p.packets[0].ack).To(Equal(ack))
			})

			It("pads Initial packets to the required minimum packet size", func() {
				token := []byte("initial token")
				packer.SetToken(token)
				f := &wire.CryptoFrame{Data: []byte("foobar")}
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x42))
				sealingManager.EXPECT().GetInitialSealer().Return(getSealer(), nil)
				sealingManager.EXPECT().GetHandshakeSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				sealingManager.EXPECT().Get0RTTSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				sealingManager.EXPECT().Get1RTTSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionInitial, false)
				initialStream.EXPECT().HasData().Return(true).Times(2)
				initialStream.EXPECT().PopCryptoFrame(gomock.Any()).Return(f)
				packer.perspective = protocol.PerspectiveClient
				p, err := packer.PackCoalescedPacket(protocol.MaxByteCount)
				Expect(err).ToNot(HaveOccurred())
				Expect(p.buffer.Len()).To(BeNumerically(">=", protocol.MinInitialPacketSize))
				Expect(p.buffer.Len()).To(BeEquivalentTo(maxPacketSize))
				Expect(p.packets).To(HaveLen(1))
				Expect(p.packets[0].header.Token).To(Equal(token))
				Expect(p.packets[0].frames).To(HaveLen(1))
				cf := p.packets[0].frames[0].Frame.(*wire.CryptoFrame)
				Expect(cf.Data).To(Equal([]byte("foobar")))
			})

			It("adds an ACK frame", func() {
				f := &wire.CryptoFrame{Data: []byte("foobar")}
				ack := &wire.AckFrame{AckRanges: []wire.AckRange{{Smallest: 42, Largest: 1337}}}
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x42))
				sealingManager.EXPECT().GetInitialSealer().Return(getSealer(), nil)
				sealingManager.EXPECT().GetHandshakeSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				sealingManager.EXPECT().Get0RTTSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				sealingManager.EXPECT().Get1RTTSealer().Return(nil, handshake.ErrKeysNotYetAvailable)
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionInitial, false).Return(ack)
				initialStream.EXPECT().HasData().Return(true).Times(2)
				initialStream.EXPECT().PopCryptoFrame(gomock.Any()).Return(f)
				packer.version = protocol.VersionTLS
				packer.perspective = protocol.PerspectiveClient
				p, err := packer.PackCoalescedPacket(protocol.MaxByteCount)
				Expect(err).ToNot(HaveOccurred())
				Expect(p.packets).To(HaveLen(1))
				Expect(p.packets[0].ack).To(Equal(ack))
				Expect(p.packets[0].frames).To(HaveLen(1))
				Expect(p.buffer.Len()).To(BeEquivalentTo(maxPacketSize))
			})
		})

		Context("packing probe packets", func() {
			It("packs an Initial probe packet", func() {
				f := &wire.CryptoFrame{Data: []byte("Initial")}
				retransmissionQueue.AddInitial(f)
				sealingManager.EXPECT().GetInitialSealer().Return(getSealer(), nil)
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionInitial, false)
				initialStream.EXPECT().HasData()
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x42))

				packet, err := packer.MaybePackProbePacket(protocol.EncryptionInitial)
				Expect(err).ToNot(HaveOccurred())
				Expect(packet).ToNot(BeNil())
				Expect(packet.EncryptionLevel()).To(Equal(protocol.EncryptionInitial))
				Expect(packet.frames).To(HaveLen(1))
				Expect(packet.frames[0].Frame).To(Equal(f))
				Expect(packet.buffer.Len()).To(BeNumerically("<", protocol.MinInitialPacketSize))
				checkLength(packet.buffer.Data)
			})

			It("packs an Initial probe packet and pads it, for the client", func() {
				packer.perspective = protocol.PerspectiveClient
				f := &wire.CryptoFrame{Data: []byte("Initial")}
				retransmissionQueue.AddInitial(f)
				sealingManager.EXPECT().GetInitialSealer().Return(getSealer(), nil)
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionInitial, false)
				initialStream.EXPECT().HasData()
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionInitial).Return(protocol.PacketNumber(0x42))

				packet, err := packer.MaybePackProbePacket(protocol.EncryptionInitial)
				Expect(err).ToNot(HaveOccurred())
				Expect(packet).ToNot(BeNil())
				Expect(packet.EncryptionLevel()).To(Equal(protocol.EncryptionInitial))
				Expect(packet.buffer.Len()).To(BeNumerically(">=", protocol.MinInitialPacketSize))
				Expect(packet.buffer.Len()).To(BeEquivalentTo(maxPacketSize))
				Expect(packet.frames).To(HaveLen(1))
				Expect(packet.frames[0].Frame).To(Equal(f))
			})

			It("packs a Handshake probe packet", func() {
				f := &wire.CryptoFrame{Data: []byte("Handshake")}
				retransmissionQueue.AddHandshake(f)
				sealingManager.EXPECT().GetHandshakeSealer().Return(getSealer(), nil)
				ackFramer.EXPECT().GetAckFrame(protocol.EncryptionHandshake, false)
				handshakeStream.EXPECT().HasData()
				pnManager.EXPECT().PeekPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.EncryptionHandshake).Return(protocol.PacketNumber(0x42))

				packet, err := packer.MaybePackProbePacket(protocol.EncryptionHandshake)
				Expect(err).ToNot(HaveOccurred())
				Expect(packet).ToNot(BeNil())
				Expect(packet.EncryptionLevel()).To(Equal(protocol.EncryptionHandshake))
				Expect(packet.frames).To(HaveLen(1))
				Expect(packet.frames[0].Frame).To(Equal(f))
				checkLength(packet.buffer.Data)
			})

			It("packs a 1-RTT probe packet", func() {
				f := &wire.StreamFrame{Data: []byte("1-RTT")}
				retransmissionQueue.AddInitial(f)
				sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
				ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, false)
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				pnManager.EXPECT().PopPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42))
				framer.EXPECT().HasData().Return(true)
				expectAppendControlFrames()
				expectAppendStreamFrames(ackhandler.Frame{Frame: f})

				packet, err := packer.MaybePackProbePacket(protocol.Encryption1RTT)
				Expect(err).ToNot(HaveOccurred())
				Expect(packet).ToNot(BeNil())
				Expect(packet.EncryptionLevel()).To(Equal(protocol.Encryption1RTT))
				Expect(packet.frames).To(HaveLen(1))
				Expect(packet.frames[0].Frame).To(Equal(f))
			})

			It("returns nil if there's no probe data to send", func() {
				sealingManager.EXPECT().Get1RTTSealer().Return(getSealer(), nil)
				ackFramer.EXPECT().GetAckFrame(protocol.Encryption1RTT, true)
				pnManager.EXPECT().PeekPacketNumber(protocol.Encryption1RTT).Return(protocol.PacketNumber(0x42), protocol.PacketNumberLen2)
				framer.EXPECT().HasData()

				packet, err := packer.MaybePackProbePacket(protocol.Encryption1RTT)
				Expect(err).ToNot(HaveOccurred())
				Expect(packet).To(BeNil())
			})
		})
	})
})

var _ = Describe("Converting to AckHandler packets", func() {
	It("convert a packet", func() {
		packet := &packetContents{
			header: &wire.ExtendedHeader{Header: wire.Header{}},
			frames: []ackhandler.Frame{{Frame: &wire.MaxDataFrame{}}, {Frame: &wire.PingFrame{}}},
			ack:    &wire.AckFrame{AckRanges: []wire.AckRange{{Largest: 100, Smallest: 80}}},
			length: 42,
		}
		t := time.Now()
		p := packet.ToAckHandlerPacket(t, nil)
		Expect(p.Length).To(Equal(protocol.ByteCount(42)))
		Expect(p.Frames).To(Equal(packet.frames))
		Expect(p.LargestAcked).To(Equal(protocol.PacketNumber(100)))
		Expect(p.SendTime).To(Equal(t))
	})

	It("sets the LargestAcked to invalid, if the packet doesn't have an ACK frame", func() {
		packet := &packetContents{
			header: &wire.ExtendedHeader{Header: wire.Header{}},
			frames: []ackhandler.Frame{{Frame: &wire.MaxDataFrame{}}, {Frame: &wire.PingFrame{}}},
		}
		p := packet.ToAckHandlerPacket(time.Now(), nil)
		Expect(p.LargestAcked).To(Equal(protocol.InvalidPacketNumber))
	})

	DescribeTable(
		"doesn't overwrite the OnLost callback, if it is set",
		func(hdr wire.Header) {
			var pingLost bool
			packet := &packetContents{
				header: &wire.ExtendedHeader{Header: hdr},
				frames: []ackhandler.Frame{
					{Frame: &wire.MaxDataFrame{}},
					{Frame: &wire.PingFrame{}, OnLost: func(wire.Frame) { pingLost = true }},
				},
			}
			p := packet.ToAckHandlerPacket(time.Now(), newRetransmissionQueue(protocol.VersionTLS))
			Expect(p.Frames).To(HaveLen(2))
			Expect(p.Frames[0].OnLost).ToNot(BeNil())
			p.Frames[1].OnLost(nil)
			Expect(pingLost).To(BeTrue())
		},
		Entry(protocol.EncryptionInitial.String(), wire.Header{IsLongHeader: true, Type: protocol.PacketTypeInitial}),
		Entry(protocol.EncryptionHandshake.String(), wire.Header{IsLongHeader: true, Type: protocol.PacketTypeHandshake}),
		Entry(protocol.Encryption0RTT.String(), wire.Header{IsLongHeader: true, Type: protocol.PacketType0RTT}),
		Entry(protocol.Encryption1RTT.String(), wire.Header{}),
	)
})
