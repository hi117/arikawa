package ws

import (
	"io"
	"net/http"

	"github.com/hi117/arikawa/v3/utils/json"
	"github.com/pkg/errors"
)

// Codec holds the codec states for Websocket implementations to share with the
// manager. It is used internally in the Websocket and the Connection
// implementation.
type Codec struct {
	Unmarshalers OpUnmarshalers
	Headers      http.Header
}

// NewCodec creates a new default Codec instance.
func NewCodec(unmarshalers OpUnmarshalers) Codec {
	return Codec{
		Unmarshalers: unmarshalers,
		Headers: http.Header{
			"Accept-Encoding": {"zlib"},
		},
	}
}

type codecOp struct {
	Op
	Data json.Raw `json:"d,omitempty"`
}

const maxSharedBufferSize = 1 << 15 // 32KB

// DecodeBuffer boxes a byte slice to provide a shared and thread-unsafe buffer.
// It is used internally and should only be handled around as an opaque thing.
type DecodeBuffer struct {
	buf []byte
}

// NewDecodeBuffer creates a new preallocated DecodeBuffer.
func NewDecodeBuffer(cap int) DecodeBuffer {
	if cap > maxSharedBufferSize {
		cap = maxSharedBufferSize
	}

	return DecodeBuffer{
		buf: make([]byte, 0, cap),
	}
}

// DecodeFrom reads the given reader and decodes it into an Op.
//
// buf is optional.
func (c Codec) DecodeFrom(r io.Reader, buf *DecodeBuffer) Op {
	var op codecOp
	op.Data = json.Raw(buf.buf)

	if err := json.DecodeStream(r, &op); err != nil {
		return newErrOp(err, "cannot read JSON stream")
	}

	// buf isn't grown from here out. Set it back right now. If Data hasn't been
	// grown, then this will just set buf back to what it was.
	if cap(op.Data) < maxSharedBufferSize {
		buf.buf = op.Data[:0]
	}

	fn := c.Unmarshalers.Lookup(op.Code, op.Type)
	if fn == nil {
		err := UnknownEventError{
			Op:   op.Code,
			Type: op.Type,
		}
		return newErrOp(err, "")
	}

	op.Op.Data = fn()
	if err := op.Data.UnmarshalTo(op.Op.Data); err != nil {
		return newErrOp(err, "cannot unmarshal JSON data from gateway")
	}

	return op.Op
}

func newErrOp(err error, wrap string) Op {
	if wrap != "" {
		err = errors.Wrap(err, wrap)
	}

	ev := &BackgroundErrorEvent{
		Err: err,
	}

	return Op{
		Code: ev.Op(),
		Type: ev.EventType(),
		Data: ev,
	}
}
