package rftp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
)

// msgs types
const (
	msgClientRequest uint8 = iota
	msgServerMetadata
	msgServerPayload
	msgClientAck
	msgClose
)

// status, the server puts to metadata
type MetaDataStatus uint8

const (
	noErr MetaDataStatus = iota
	fileNotExistent
	fileEmpty
	accessDenied
)

func (m MetaDataStatus) String() string {
	switch uint8(m) {
	case 0:
		return "0: no error"
	case 1:
		return "1: file does not exist"
	case 2:
		return "2: file is empty"
	case 3:
		return "3: access denied"
	case 4:
		return "4: Offset bigger than filesize"
	}
	return fmt.Sprintf("unknown error: %v", uint8(m))
}

type option struct {
	otype uint8
	value []byte

	// Length of serialized struct in byte. Is not used during serialization,
	// i.e., it does not need to be set if a new struct is populated and //
	// serialized.
	length int
}

func (o *option) UnmarshalBinary(data []byte) error {
	if len(data) < 2 {
		return fmt.Errorf("option too short")
	}

	o.otype = data[0]
	valueLen := uint8(data[1])
	o.length = 2 + int(valueLen)
	if len(data) < o.length {
		return fmt.Errorf("data slice too small: expected %d, got %d",
			o.length, len(data))
	}
	o.value = data[2:o.length]

	return nil
}

func (o *option) MarshalBinary() (data []byte, err error) {
	buf := make([]byte, 2+len(o.value))
	buf[0] = o.otype
	buf[1] = byte(len(o.value))
	copy(buf[2:], o.value)
	return buf, nil
}

type msgHeader struct {
	version   uint8
	msgType   uint8
	ackNum    uint8
	optionLen uint8
	options   []option

	hdrLen int
}

func (s msgHeader) MarshalBinary() ([]byte, error) {
	buf := new(bytes.Buffer)
	vt := s.version<<4 ^ s.msgType
	err := binary.Write(buf, binary.BigEndian, vt)
	if err != nil {
		return nil, err
	}
	err = binary.Write(buf, binary.BigEndian, s.ackNum)
	if err != nil {
		return nil, err
	}
	err = binary.Write(buf, binary.BigEndian, s.optionLen)
	if err != nil {
		return nil, err
	}
	for _, o := range s.options {
		ob, err := o.MarshalBinary()
		if err != nil {
			return nil, err
		}
		buf.Write(ob)
	}

	return buf.Bytes(), nil
}

func (s *msgHeader) UnmarshalBinary(data []byte) error {
	if len(data) < 2 {
		return fmt.Errorf("MsgHeader too short")
	}
	vt := uint8(data[0])
	s.version = vt & 0xF0 >> 4
	s.msgType = vt & 0x0F
	s.ackNum = uint8(data[1])
	s.optionLen = uint8(data[2])
	if s.optionLen > 0 {
		s.options = make([]option, s.optionLen)
	}

	s.hdrLen = 3

	lens := data[3:]
	for i := 0; uint8(i) < s.optionLen; i++ {
		o := option{}
		if err := o.UnmarshalBinary(lens); err != nil {
			return err
		}
		s.options[i] = o
		s.hdrLen += o.length
		lens = lens[o.length:]
	}

	return nil
}

type clientRequest struct {
	maxTransmissionRate uint32
	files               []fileDescriptor
}

type fileDescriptor struct {
	offset   uint64
	fileName string
}

var maxFileOffset = uint64(math.Pow(2, 56)) - 1

func (s clientRequest) MarshalBinary() ([]byte, error) {
	buf := new(bytes.Buffer)

	err := binary.Write(buf, binary.BigEndian, s.maxTransmissionRate)
	if err != nil {
		return nil, err
	}
	err = binary.Write(buf, binary.BigEndian, uint16(len(s.files)))
	if err != nil {
		return nil, err
	}

	for _, file := range s.files {
		if file.offset > maxFileOffset {
			return nil, errors.New("file offset to big")
		}

		sb, err := sevenByteOffset(file.offset)
		if err != nil {
			return nil, err
		}
		err = binary.Write(buf, binary.BigEndian, sb)
		if err != nil {
			return nil, err
		}

		pathBin := []byte(file.fileName)
		err = binary.Write(buf, binary.BigEndian, uint16(len(pathBin)))
		if err != nil {
			return nil, err
		}
		_, err = buf.Write(pathBin)
		if err != nil {
			return nil, err
		}
	}

	return buf.Bytes(), nil
}

func (s *clientRequest) UnmarshalBinary(data []byte) error {
	s.maxTransmissionRate = binary.BigEndian.Uint32(data[:4])
	numFiles := binary.BigEndian.Uint16(data[4:6])

	if numFiles == 0 {
		return nil
	}

	s.files = make([]fileDescriptor, numFiles)

	dataLens := data[6:]
	for i := uint16(0); i < numFiles; i++ {
		f := fileDescriptor{}
		f.offset = uintOffset(dataLens[:7])
		pathLen := binary.BigEndian.Uint16(dataLens[7:9])
		f.fileName = string(dataLens[9 : 9+pathLen])
		dataLens = dataLens[9+pathLen:]
		s.files[i] = f
	}

	return nil
}

type serverMetaData struct {
	ackNum    uint8
	status    MetaDataStatus
	fileIndex uint16
	size      uint64
	checkSum  [16]byte
}

func (s serverMetaData) MarshalBinary() ([]byte, error) {
	buf := new(bytes.Buffer)
	err := binary.Write(buf, binary.BigEndian, byte(0))
	if err != nil {
		return nil, err
	}
	err = binary.Write(buf, binary.BigEndian, s.status)
	if err != nil {
		return nil, err
	}
	err = binary.Write(buf, binary.BigEndian, s.fileIndex)
	if err != nil {
		return nil, err
	}
	err = binary.Write(buf, binary.BigEndian, s.size)
	if err != nil {
		return nil, err
	}
	_, err = buf.Write(s.checkSum[:])
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), err
}

func (s *serverMetaData) UnmarshalBinary(data []byte) error {
	s.status = MetaDataStatus(data[1])
	s.fileIndex = binary.BigEndian.Uint16(data[2:4])
	s.size = binary.BigEndian.Uint64(data[4:12])

	cs := data[12:28]

	for i, c := range cs {
		s.checkSum[i] = c
	}
	return nil
}

type serverPayload struct {
	fileIndex uint16
	ackNumber uint8
	offset    uint64
	data      []byte
}

func (s *serverPayload) String() string {
	return fmt.Sprintf("%v", *s)
}

func (s serverPayload) MarshalBinary() ([]byte, error) {
	buf := new(bytes.Buffer)
	err := binary.Write(buf, binary.BigEndian, s.fileIndex)
	if err != nil {
		return nil, err
	}
	sb, err := sevenByteOffset(s.offset)
	if err != nil {
		return nil, err
	}
	err = binary.Write(buf, binary.BigEndian, sb)
	if err != nil {
		return nil, err
	}

	_, err = buf.Write(s.data)
	bs := buf.Bytes()
	return bs, err
}

func (s *serverPayload) UnmarshalBinary(data []byte) error {
	s.fileIndex = binary.BigEndian.Uint16(data[0:2])

	s.offset = uintOffset(data[2:9])

	if len(data) > 9 {
		s.data = data[9:]
	}
	return nil
}

type resendEntry struct {
	fileIndex uint16
	offset    uint64
	length    uint8
}

func (r *resendEntry) String() string {
	return fmt.Sprintf("%v", *r)
}

type resendEntryList []*resendEntry

// Len is the number of elements in the collection.
func (r *resendEntryList) Len() int {
	return len(*r)
}

// Less reports whether the element with
// index i should sort before the element with index j.
func (r *resendEntryList) Less(i int, j int) bool {
	return (*r)[i].offset < (*r)[j].offset
}

// Swap swaps the elements with indexes i and j.
func (r *resendEntryList) Swap(i int, j int) {
	(*r)[i], (*r)[j] = (*r)[j], (*r)[i]
}

const (
	metaDataReceived uint8 = iota
	metaDataMissing
)

type clientAck struct {
	ackNumber           uint8
	fileIndex           uint16
	status              uint8
	maxTransmissionRate uint32
	offset              uint64
	resendEntries       resendEntryList
}

func (c *clientAck) String() string {
	res := []string{}
	sort.Sort(&c.resendEntries)
	for _, re := range c.resendEntries {
		res = append(res, re.String())
	}
	return fmt.Sprintf(
		"{%v %v %v %v %v %v}",
		c.ackNumber,
		c.fileIndex,
		c.status,
		c.maxTransmissionRate,
		c.offset,
		fmt.Sprintf("[%v]", strings.Join(res, " ")),
	)
}

// make offset BigEndian and cut off the first (most significant) byte
func sevenByteOffset(offset uint64) ([]byte, error) {
	offsetBuffer := new(bytes.Buffer)
	err := binary.Write(offsetBuffer, binary.BigEndian, offset)
	if err != nil {
		return nil, err
	}
	return offsetBuffer.Bytes()[1:], nil
}

// pad 7 byte with another zero byte to make reading easy
func uintOffset(seven []byte) uint64 {
	offsetPad := append([]byte{0}, seven...)
	return binary.BigEndian.Uint64(offsetPad)
}

func (c clientAck) MarshalBinary() ([]byte, error) {
	buf := new(bytes.Buffer)
	err := binary.Write(buf, binary.BigEndian, c.fileIndex)
	if err != nil {
		return nil, err
	}
	err = binary.Write(buf, binary.BigEndian, c.status)
	if err != nil {
		return nil, err
	}
	err = binary.Write(buf, binary.BigEndian, c.maxTransmissionRate)
	if err != nil {
		return nil, err
	}

	sb, err := sevenByteOffset(c.offset)
	if err != nil {
		return nil, err
	}

	err = binary.Write(buf, binary.BigEndian, sb)
	if err != nil {
		return nil, err
	}

	for _, re := range c.resendEntries {
		err = binary.Write(buf, binary.BigEndian, re.fileIndex)
		if err != nil {
			return nil, err
		}
		sb, err = sevenByteOffset(re.offset)
		if err != nil {
			return nil, err
		}
		err = binary.Write(buf, binary.BigEndian, sb)
		if err != nil {
			return nil, err
		}
		err = binary.Write(buf, binary.BigEndian, re.length)
		if err != nil {
			return nil, err
		}
	}
	bs := buf.Bytes()
	return bs, nil
}

func (c *clientAck) UnmarshalBinary(data []byte) error {
	c.fileIndex = binary.BigEndian.Uint16(data[0:2])
	c.status = uint8(data[2])
	c.maxTransmissionRate = binary.BigEndian.Uint32(data[3:7])
	c.offset = uintOffset(data[7:14])

	if len(data) > 14 {
		reBytes := data[14:]
		for i := 0; i < len(reBytes)/10; i++ {
			re := &resendEntry{}
			re.fileIndex = binary.BigEndian.Uint16(reBytes[:2])
			re.offset = uintOffset(reBytes[2:9])
			re.length = uint8(reBytes[9])
			c.resendEntries = append(c.resendEntries, re)
			reBytes = reBytes[10:]
		}

	}
	return nil
}

type CloseConnectionReason uint16

const (
	noReason CloseConnectionReason = iota
	applicationClosed
	unsupportedVersion
	unknownRequest
	wrongChecksum
	donwloadFinished
	timeout
)

func (m CloseConnectionReason) String() string {
	switch uint8(m) {
	case 0:
		return "0: no reason provided"
	case 1:
		return "1: application closed"
	case 2:
		return "2: unsupported version"
	case 3:
		return "3: unknown request"
	case 4:
		return "4: wrong checksum"
	case 5:
		return "5: download finished"
	case 6:
		return "6: timeout"
	}
	return fmt.Sprintf("unknown reason: %v", uint8(m))
}

type closeConnection struct {
	reason CloseConnectionReason
}

func (c closeConnection) MarshalBinary() ([]byte, error) {
	buf := new(bytes.Buffer)
	err := binary.Write(buf, binary.BigEndian, c.reason)
	if err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (c *closeConnection) UnmarshalBinary(data []byte) error {
	c.reason = CloseConnectionReason(binary.BigEndian.Uint16(data[:2]))
	return nil
}
