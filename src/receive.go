package main

import (
	"bytes"
	"encoding/binary"
	"golang.org/x/crypto/chacha20poly1305"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ElementStateOkay = iota
	ElementStateDropped
)

type QueueHandshakeElement struct {
	msgType uint32
	packet  []byte
	source  *net.UDPAddr
}

type QueueInboundElement struct {
	state   uint32
	mutex   sync.Mutex
	packet  []byte
	counter uint64
	keyPair *KeyPair
}

func (elem *QueueInboundElement) Drop() {
	atomic.StoreUint32(&elem.state, ElementStateDropped)
}

func (elem *QueueInboundElement) IsDropped() bool {
	return atomic.LoadUint32(&elem.state) == ElementStateDropped
}

func addToInboundQueue(
	queue chan *QueueInboundElement,
	element *QueueInboundElement,
) {
	for {
		select {
		case queue <- element:
			return
		default:
			select {
			case old := <-queue:
				old.Drop()
			default:
			}
		}
	}
}

func (device *Device) RoutineReceiveIncomming() {

	debugLog := device.log.Debug
	debugLog.Println("Routine, receive incomming, started")

	errorLog := device.log.Error

	var buffer []byte // unsliced buffer

	for {

		// check if stopped

		select {
		case <-device.signal.stop:
			return
		default:
		}

		// read next datagram

		if buffer == nil {
			buffer = make([]byte, MaxMessageSize)
		}

		device.net.mutex.RLock()
		conn := device.net.conn
		device.net.mutex.RUnlock()
		if conn == nil {
			time.Sleep(time.Second)
			continue
		}

		conn.SetReadDeadline(time.Now().Add(time.Second))

		size, raddr, err := conn.ReadFromUDP(buffer)
		if err != nil || size < MinMessageSize {
			continue
		}

		// handle packet

		packet := buffer[:size]
		msgType := binary.LittleEndian.Uint32(packet[:4])

		func() {
			switch msgType {

			case MessageInitiationType, MessageResponseType:

				// verify mac1

				if !device.mac.CheckMAC1(packet) {
					debugLog.Println("Received packet with invalid mac1")
					return
				}

				// check if busy, TODO: refine definition of "busy"

				busy := len(device.queue.handshake) > QueueHandshakeBusySize
				if busy && !device.mac.CheckMAC2(packet, raddr) {
					sender := binary.LittleEndian.Uint32(packet[4:8]) // "sender" follows "type"
					reply, err := device.CreateMessageCookieReply(packet, sender, raddr)
					if err != nil {
						errorLog.Println("Failed to create cookie reply:", err)
						return
					}
					writer := bytes.NewBuffer(packet[:0])
					binary.Write(writer, binary.LittleEndian, reply)
					packet = writer.Bytes()
					_, err = device.net.conn.WriteToUDP(packet, raddr)
					if err != nil {
						debugLog.Println("Failed to send cookie reply:", err)
					}
					return
				}

				// add to handshake queue

				buffer = nil
				device.queue.handshake <- QueueHandshakeElement{
					msgType: msgType,
					packet:  packet,
					source:  raddr,
				}

			case MessageCookieReplyType:

				// verify and update peer cookie state

				if len(packet) != MessageCookieReplySize {
					return
				}

				var reply MessageCookieReply
				reader := bytes.NewReader(packet)
				err := binary.Read(reader, binary.LittleEndian, &reply)
				if err != nil {
					debugLog.Println("Failed to decode cookie reply")
					return
				}
				device.ConsumeMessageCookieReply(&reply)

			case MessageTransportType:

				// lookup key pair

				if len(packet) < MessageTransportSize {
					return
				}

				receiver := binary.LittleEndian.Uint32(
					packet[MessageTransportOffsetReceiver:MessageTransportOffsetCounter],
				)
				value := device.indices.Lookup(receiver)
				keyPair := value.keyPair
				if keyPair == nil {
					return
				}

				// check key-pair expiry

				if keyPair.created.Add(RejectAfterTime).Before(time.Now()) {
					return
				}

				// add to peer queue

				peer := value.peer
				work := new(QueueInboundElement)
				work.packet = packet
				work.keyPair = keyPair
				work.state = ElementStateOkay
				work.mutex.Lock()

				// add to decryption queues

				addToInboundQueue(device.queue.decryption, work)
				addToInboundQueue(peer.queue.inbound, work)
				buffer = nil

			default:
				// unknown message type
				debugLog.Println("Got unknown message from:", raddr)
			}
		}()
	}
}

func (device *Device) RoutineDecryption() {
	var elem *QueueInboundElement
	var nonce [chacha20poly1305.NonceSize]byte

	logDebug := device.log.Debug
	logDebug.Println("Routine, decryption, started for device")

	for {
		select {
		case elem = <-device.queue.decryption:
		case <-device.signal.stop:
			return
		}

		// check if dropped

		if elem.IsDropped() {
			elem.mutex.Unlock()
			continue
		}

		// split message into fields

		counter := elem.packet[MessageTransportOffsetCounter:MessageTransportOffsetContent]
		content := elem.packet[MessageTransportOffsetContent:]

		// decrypt with key-pair

		var err error
		copy(nonce[4:], counter)
		elem.counter = binary.LittleEndian.Uint64(counter)
		elem.packet, err = elem.keyPair.receive.Open(elem.packet[:0], nonce[:], content, nil)
		if err != nil {
			elem.Drop()
		}
		elem.mutex.Unlock()
	}
}

/* Handles incomming packets related to handshake
 *
 *
 */
func (device *Device) RoutineHandshake() {

	logInfo := device.log.Info
	logError := device.log.Error
	logDebug := device.log.Debug
	logDebug.Println("Routine, handshake routine, started for device")

	var elem QueueHandshakeElement

	for {
		select {
		case elem = <-device.queue.handshake:
		case <-device.signal.stop:
			return
		}

		func() {

			switch elem.msgType {
			case MessageInitiationType:

				// unmarshal

				if len(elem.packet) != MessageInitiationSize {
					return
				}

				var msg MessageInitiation
				reader := bytes.NewReader(elem.packet)
				err := binary.Read(reader, binary.LittleEndian, &msg)
				if err != nil {
					logError.Println("Failed to decode initiation message")
					return
				}

				// consume initiation

				peer := device.ConsumeMessageInitiation(&msg)
				if peer == nil {
					logInfo.Println(
						"Recieved invalid initiation message from",
						elem.source.IP.String(),
						elem.source.Port,
					)
					return
				}
				logDebug.Println("Recieved valid initiation message for peer", peer.id)

			case MessageResponseType:

				// unmarshal

				if len(elem.packet) != MessageResponseSize {
					return
				}

				var msg MessageResponse
				reader := bytes.NewReader(elem.packet)
				err := binary.Read(reader, binary.LittleEndian, &msg)
				if err != nil {
					logError.Println("Failed to decode response message")
					return
				}

				// consume response

				peer := device.ConsumeMessageResponse(&msg)
				if peer == nil {
					logInfo.Println(
						"Recieved invalid response message from",
						elem.source.IP.String(),
						elem.source.Port,
					)
					return
				}
				sendSignal(peer.signal.handshakeCompleted)
				logDebug.Println("Recieved valid response message for peer", peer.id)
				kp := peer.NewKeyPair()
				if kp == nil {
					logDebug.Println("Failed to derieve key-pair")
				}
				peer.SendKeepAlive()

			default:
				device.log.Error.Println("Invalid message type in handshake queue")
			}
		}()
	}
}

func (peer *Peer) RoutineSequentialReceiver() {
	var elem *QueueInboundElement

	device := peer.device
	logDebug := device.log.Debug
	logDebug.Println("Routine, sequential receiver, started for peer", peer.id)

	for {
		// wait for decryption

		select {
		case <-peer.signal.stop:
			return
		case elem = <-peer.queue.inbound:
		}

		elem.mutex.Lock()
		if elem.IsDropped() {
			continue
		}

		// check for replay

		// update timers

		// check for keep-alive

		if len(elem.packet) == 0 {
			continue
		}

		// strip padding

		// insert into inbound TUN queue

		device.queue.inbound <- elem.packet

		// update key material
	}
}

func (device *Device) RoutineWriteToTUN(tun TUNDevice) {
	logError := device.log.Error
	logDebug := device.log.Debug
	logDebug.Println("Routine, sequential tun writer, started")

	for {
		select {
		case <-device.signal.stop:
			return
		case packet := <-device.queue.inbound:
			_, err := tun.Write(packet)
			if err != nil {
				logError.Println("Failed to write packet to TUN device:", err)
			}
		}
	}
}