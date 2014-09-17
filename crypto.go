package weave

import (
	"bytes"
	"code.google.com/p/go-bit/bit"
	"code.google.com/p/go.crypto/nacl/box"
	"code.google.com/p/go.crypto/nacl/secretbox"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"fmt"
	"log"
)

func GenerateKeyPair() (publicKey, privateKey *[32]byte, err error) {
	return box.GenerateKey(rand.Reader)
}

func FormSessionKey(remotePublicKey, localPrivateKey *[32]byte, secretKey *[]byte) *[32]byte {
	var sharedKey [32]byte
	box.Precompute(&sharedKey, remotePublicKey, localPrivateKey)
	sharedKeySlice := sharedKey[:]
	sharedKeySlice = append(sharedKeySlice, *secretKey...)
	sessionKey := sha256.Sum256(sharedKeySlice)
	return &sessionKey
}

func GenerateRandomNonce() ([24]byte, error) {
	var nonce [24]byte
	n, err := rand.Read(nonce[:])
	if err != nil {
		return nonce, err
	}
	if n != 24 {
		return nonce, fmt.Errorf("Did not read enough - wanted 24, got %v", n)
	}
	return nonce, nil
}

func EncryptPrefixNonce(plaintxt []byte, nonce *[24]byte, secret *[32]byte) []byte {
	buf := make([]byte, 24, 24+len(plaintxt)+secretbox.Overhead)
	copy(buf, nonce[:])
	// Seal *appends* to buf
	return secretbox.Seal(buf, plaintxt, nonce, secret)
}

func DecryptPrefixNonce(ciphertxt []byte, secret *[32]byte) ([]byte, bool) {
	if len(ciphertxt) < secretbox.Overhead+24 {
		return nil, false
	}
	// There is no way to nicely convert from a slice to an
	// array. So have to used the following loop.
	var nonce [24]byte
	for idx, e := range ciphertxt[0:24] {
		nonce[idx] = e
	}
	ciphertxt = ciphertxt[24:]
	return secretbox.Open(nil, ciphertxt, &nonce, secret)
}

func SetNonceLow15Bits(nonce *[24]byte, offset uint16) {
	// ensure top bit of offset is 0
	offset = offset & ((1 << 15) - 1)
	// grab top bit of nonce[22:24] (and clear out lower bits)
	nonceBits := binary.BigEndian.Uint16(nonce[22:24]) & (1 << 15)
	// Big endian => the MSB is stored at the *lowest* address. So
	// that top bit in nonce[22] should stay as the top bit in
	// nonce[22]
	binary.BigEndian.PutUint16(nonce[22:24], nonceBits+offset)
}

// Nonce encoding/decoding

func EncodeNonce(df bool) (*[24]byte, []byte, error) {
	nonce, err := GenerateRandomNonce()
	if err != nil {
		return nil, []byte{}, err
	}
	// wipe out lowest 15 bits, but encode the df right at the bottom
	flags := uint16(0)
	if df {
		flags = flags | 1
	}
	SetNonceLow15Bits(&nonce, flags)
	return &nonce, Concat(ProtocolNonceByte, nonce[:]), nil
}

func DecodeNonce(msg []byte) (bool, *[24]byte) {
	flags := uint16(msg[23])
	df := 0 != (flags & 1)
	nonce := [24]byte{}
	// upper bound is exclusive so this avoids copying the flags
	for idx, elem := range msg[:23] {
		nonce[idx] = elem
	}
	return df, &nonce
}

// Frame Encryptors

func NewNonEncryptor(prefix []byte) *NonEncryptor {
	buf := make([]byte, MaxUDPPacketSize)
	prefixLen := copy(buf, prefix)
	return &NonEncryptor{
		buf:       buf,
		bufTail:   buf[prefixLen:],
		buffered:  prefixLen,
		prefixLen: prefixLen}
}

func (ne *NonEncryptor) PacketOverhead() int {
	return ne.prefixLen
}

func (ne *NonEncryptor) FrameOverhead() int {
	return NameSize + NameSize + 2
}

func (ne *NonEncryptor) IsEmpty() bool {
	return ne.buffered == ne.prefixLen
}

func (ne *NonEncryptor) Bytes() []byte {
	buf := ne.buf[:ne.buffered]
	ne.buffered = ne.prefixLen
	ne.bufTail = ne.buf[ne.prefixLen:]
	return buf
}

func (ne *NonEncryptor) AppendFrame(frame *ForwardedFrame) {
	bufTail := ne.bufTail
	srcLen := copy(bufTail, frame.srcPeer.NameByte)
	bufTail = bufTail[srcLen:]
	dstLen := copy(bufTail, frame.dstPeer.NameByte)
	bufTail = bufTail[dstLen:]
	frameLen := len(frame.frame)
	binary.BigEndian.PutUint16(bufTail, uint16(frameLen))
	bufTail = bufTail[2:]
	copy(bufTail, frame.frame)
	ne.bufTail = bufTail[frameLen:]
	ne.buffered += srcLen + dstLen + 2 + frameLen
}

func (ne *NonEncryptor) TotalLen() int {
	return ne.buffered
}

func NewNaClEncryptor(prefix []byte, conn *LocalConnection, df bool) *NaClEncryptor {
	buf := make([]byte, MaxUDPPacketSize)
	prefixLen := copy(buf, prefix)
	flags := uint16(0)
	if df {
		flags = flags | (1 << 15)
	}
	return &NaClEncryptor{
		NonEncryptor: *NewNonEncryptor([]byte{}),
		buf:          buf,
		offset:       0,
		nonce:        nil,
		nonceChan:    make(chan *[24]byte, ChannelSize),
		flags:        flags,
		prefixLen:    prefixLen,
		conn:         conn,
		df:           df}
}

func (ne *NaClEncryptor) Bytes() []byte {
	plaintext := ne.NonEncryptor.Bytes()
	offsetFlags := ne.offset | ne.flags
	ciphertext := ne.buf
	binary.BigEndian.PutUint16(ciphertext[ne.prefixLen:], offsetFlags)
	nonce := ne.nonce
	if nonce == nil {
		freshNonce, encodedNonce, err := EncodeNonce(ne.df)
		if err = ne.conn.CheckFatal(err); err != nil {
			return []byte{}
		}
		ne.conn.SendTCP(encodedNonce)
		ne.nonce = freshNonce
		nonce = freshNonce
	}
	offset := ne.offset
	SetNonceLow15Bits(nonce, offset)
	// Seal *appends* to ciphertext
	ciphertext = secretbox.Seal(ciphertext[:ne.prefixLen+2], plaintext, nonce, ne.conn.SessionKey)

	offset = (offset + 1) & ((1 << 15) - 1)
	if offset == 0 {
		// need a new nonce please
		ne.nonce = <-ne.nonceChan
	} else if offset == 1<<14 { // half way through range, send new nonce
		nonce, encodedNonce, err := EncodeNonce(ne.df)
		if err = ne.conn.CheckFatal(err); err != nil {
			return []byte{}
		}
		ne.nonceChan <- nonce
		ne.conn.SendTCP(encodedNonce)
	}
	ne.offset = offset

	return ciphertext
}

func (ne *NaClEncryptor) PacketOverhead() int {
	return ne.prefixLen + 2 + secretbox.Overhead + ne.NonEncryptor.PacketOverhead()
}

func (ne *NaClEncryptor) TotalLen() int {
	return ne.PacketOverhead() + ne.NonEncryptor.TotalLen()
}

// Frame Decryptors

func NewNonDecryptor(conn *LocalConnection) *NonDecryptor {
	return &NonDecryptor{conn: conn}
}

func (nd *NonDecryptor) IterateFrames(fun FrameConsumer, packet *UDPPacket) error {
	buf := packet.Packet
	for len(buf) >= (2 + NameSize + NameSize) {
		srcNameByte := buf[:NameSize]
		buf = buf[NameSize:]
		dstNameByte := buf[:NameSize]
		buf = buf[NameSize:]
		length := binary.BigEndian.Uint16(buf[:2])
		frame := buf[2 : 2+length]
		buf = buf[2+length:]
		err := fun(nd.conn, packet.Sender, srcNameByte, dstNameByte, length, frame)
		if err != nil {
			return err
		}
	}
	return nil
}

func (nd *NonDecryptor) Shutdown() {
}

func (nd *NonDecryptor) ReceiveNonce(msg []byte) {
	log.Println("Received Nonce on non-encrypted channel. Ignoring.")
}

func NewNaClDecryptor(conn *LocalConnection) *NaClDecryptor {
	inst := NaClDecryptorInstance{
		nonce:               nil,
		previousNonce:       nil,
		usedOffsets:         bit.New(),
		previousUsedOffsets: nil,
		highestOffsetSeen:   0,
		nonceChan:           make(chan *[24]byte, ChannelSize)}
	instDF := NaClDecryptorInstance{
		nonce:               nil,
		previousNonce:       nil,
		usedOffsets:         bit.New(),
		previousUsedOffsets: nil,
		highestOffsetSeen:   0,
		nonceChan:           make(chan *[24]byte, ChannelSize)}
	return &NaClDecryptor{
		NonDecryptor: *NewNonDecryptor(conn),
		instance:     &inst,
		instanceDF:   &instDF}
}

func (nd *NaClDecryptor) Shutdown() {
	close(nd.instance.nonceChan)
	close(nd.instanceDF.nonceChan)
}

func (nd *NaClDecryptor) ReceiveNonce(msg []byte) {
	df, nonce := DecodeNonce(msg)
	if df {
		nd.instanceDF.nonceChan <- nonce
	} else {
		nd.instance.nonceChan <- nonce
	}
}

func (nd *NaClDecryptor) IterateFrames(fun FrameConsumer, packet *UDPPacket) error {
	buf, err := nd.decrypt(packet.Packet)
	if err != nil {
		return err
	}
	packet.Packet = buf
	return nd.NonDecryptor.IterateFrames(fun, packet)
}

func (nd *NaClDecryptor) decrypt(buf []byte) ([]byte, error) {
	offset := binary.BigEndian.Uint16(buf[:2])
	df := (offset & (1 << 15)) != 0
	offsetNoFlags := offset & ((1 << 15) - 1)
	var decState *NaClDecryptorInstance
	if df {
		decState = nd.instanceDF
	} else {
		decState = nd.instance
	}
	var nonce *[24]byte
	var usedOffsets *bit.Set
	var ok bool
	if decState.nonce == nil {
		if offsetNoFlags > (1 << 13) {
			// offset is already beyond the first quarter and it's the
			// first thing we've seen?! I don't think so.
			return nil, fmt.Errorf("Unexpected offset when decrypting UDP packet")
		}
		decState.nonce, ok = <-decState.nonceChan
		if !ok {
			return nil, fmt.Errorf("Nonce chan closed")
		}
		nonce = decState.nonce
		usedOffsets = decState.usedOffsets
		decState.highestOffsetSeen = offsetNoFlags
	} else {
		highestOffsetSeen := decState.highestOffsetSeen
		if offsetNoFlags < (1<<13) && highestOffsetSeen > ((1<<14)+(1<<13)) &&
			(highestOffsetSeen-offsetNoFlags) > ((1<<14)+(1<<13)) {
			// offset is in the first quarter, highestOffsetSeen is in
			// the top quarter and under a quarter behind us. We
			// interpret this as we need to move to the next nonce
			decState.previousUsedOffsets = decState.usedOffsets
			decState.usedOffsets = bit.New()
			decState.previousNonce = decState.nonce
			decState.nonce, ok = <-decState.nonceChan
			if !ok {
				return nil, fmt.Errorf("Nonce chan closed")
			}
			decState.highestOffsetSeen = offsetNoFlags
			nonce = decState.nonce
			usedOffsets = decState.usedOffsets
		} else if offsetNoFlags > highestOffsetSeen &&
			(offsetNoFlags-highestOffsetSeen) < (1<<13) {
			// offset is under a quarter above highestOffsetSeen. This
			// is ok - maybe some packet loss
			decState.highestOffsetSeen = offsetNoFlags
			nonce = decState.nonce
			usedOffsets = decState.usedOffsets
		} else if offsetNoFlags <= highestOffsetSeen &&
			(highestOffsetSeen-offsetNoFlags) < (1<<13) {
			// offset is within a quarter of the highest we've
			// seen. This is ok - just assuming some out-of-order
			// delivery.
			nonce = decState.nonce
			usedOffsets = decState.usedOffsets
		} else if highestOffsetSeen < (1<<13) && offsetNoFlags > ((1<<14)+(1<<13)) &&
			(offsetNoFlags-highestOffsetSeen) > ((1<<14)+(1<<13)) {
			// offset is in the last quarter, highestOffsetSeen is in
			// the first quarter, and offset is under a quarter behind
			// us. This is ok - as above, just some out of order. But
			// here it means we're dealing with the previous nonce
			nonce = decState.previousNonce
			usedOffsets = decState.previousUsedOffsets
		} else {
			return nil, fmt.Errorf("Unexpected offset when decrypting UDP packet")
		}
	}
	offsetNoFlagsInt := int(offsetNoFlags)
	if usedOffsets.Contains(offsetNoFlagsInt) {
		return nil, fmt.Errorf("Suspected replay attack detected when decrypting UDP packet")
	}
	SetNonceLow15Bits(nonce, offsetNoFlags)
	result, success := secretbox.Open(nil, buf[2:], nonce, nd.conn.SessionKey)
	if success {
		usedOffsets.Add(offsetNoFlagsInt)
		return result, nil
	} else {
		return nil, fmt.Errorf("Unable to decrypt msg via UDP: %v", buf)
	}
}

// TCP Senders

func NewSimpleTCPSender(encoder *gob.Encoder) *SimpleTCPSender {
	return &SimpleTCPSender{encoder: encoder}
}

func (sender *SimpleTCPSender) Send(msg []byte) error {
	return sender.encoder.Encode(msg)
}

func NewEncryptedTCPSender(encoder *gob.Encoder, conn *LocalConnection) *EncryptedTCPSender {
	buffer := new(bytes.Buffer)
	return &EncryptedTCPSender{
		outerEncoder: encoder,
		innerEncoder: gob.NewEncoder(buffer),
		buffer:       buffer,
		conn:         conn,
		msgCount:     0}
}

func (sender *EncryptedTCPSender) Send(msg []byte) error {
	nonce, err := GenerateRandomNonce()
	if err != nil {
		return err
	}
	sender.Lock()
	defer sender.Unlock()
	wrappedMsg := EncryptedTCPMessage{Number: sender.msgCount, Body: msg}
	buffer := sender.buffer
	buffer.Reset()
	err = sender.innerEncoder.Encode(wrappedMsg)
	if err != nil {
		return err
	}
	sender.msgCount = sender.msgCount + 1
	return sender.outerEncoder.Encode(
		EncryptPrefixNonce(buffer.Bytes(), &nonce, sender.conn.SessionKey))
}

// TCP Receivers

func NewSimpleTCPReceiver() *SimpleTCPReceiver {
	return &SimpleTCPReceiver{}
}

func (receiver *SimpleTCPReceiver) Decode(msg []byte) ([]byte, error) {
	return msg, nil
}

func NewEncryptedTCPReceiver(conn *LocalConnection) *EncryptedTCPReceiver {
	buffer := new(bytes.Buffer)
	return &EncryptedTCPReceiver{
		conn:     conn,
		decoder:  gob.NewDecoder(buffer),
		buffer:   buffer,
		msgCount: 0}
}

func (receiver *EncryptedTCPReceiver) Decode(msg []byte) ([]byte, error) {
	plaintext, success := DecryptPrefixNonce(msg, receiver.conn.SessionKey)
	if !success {
		return msg, fmt.Errorf("Unable to decrypt msg via TCP:\n %X", msg)
	}
	receiver.buffer.Reset()
	_, err := receiver.buffer.Write(plaintext)
	if err != nil {
		return msg, err
	}
	wrappedMsg := new(EncryptedTCPMessage)
	err = receiver.decoder.Decode(wrappedMsg)
	if err != nil {
		return msg, err
	}
	if wrappedMsg.Number != receiver.msgCount {
		return msg, fmt.Errorf("Received TCP message with wrong sequence number; possible replay attack")
	}
	receiver.msgCount = receiver.msgCount + 1
	return wrappedMsg.Body, nil
}
