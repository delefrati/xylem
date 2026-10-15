package cache

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// frame is a message the proxy forwards without decoding.
type frame struct{ data []byte }

// codec marshals proto messages as usual and passes frames through, so one
// server can host typed services and the byte-level proxy.
type codec struct{}

func (codec) Name() string { return "proto" }

func (codec) Marshal(v any) ([]byte, error) {
	switch m := v.(type) {
	case *frame:
		return m.data, nil
	case proto.Message:
		return proto.Marshal(m)
	}
	return nil, fmt.Errorf("cannot marshal %T", v)
}

func (codec) Unmarshal(data []byte, v any) error {
	switch m := v.(type) {
	case *frame:
		m.data = append(m.data[:0], data...)
		return nil
	case proto.Message:
		return proto.Unmarshal(data, m)
	}
	return fmt.Errorf("cannot unmarshal into %T", v)
}

var _ encoding.Codec = codec{}

// ProxiedServices are forwarded by the daemon to the server unchanged.
var ProxiedServices = []string{"/xylem.v1.RepoService/", "/xylem.v1.WriteService/"}

// Proxy returns server options that forward calls to ProxiedServices over
// upstream byte for byte (metadata, messages, headers, trailers and status).
// Other unknown methods are Unimplemented.
func Proxy(upstream *grpc.ClientConn) []grpc.ServerOption {
	handler := func(_ any, ss grpc.ServerStream) error {
		method, ok := grpc.MethodFromServerStream(ss)
		if !ok {
			return status.Error(codes.Internal, "proxy: no method")
		}
		proxied := false
		for _, p := range ProxiedServices {
			proxied = proxied || strings.HasPrefix(method, p)
		}
		if !proxied {
			return status.Errorf(codes.Unimplemented, "unknown method %s", method)
		}
		ctx := ss.Context()
		md, _ := metadata.FromIncomingContext(ctx)
		md = md.Copy()
		delete(md, ":authority")
		out, err := upstream.NewStream(metadata.NewOutgoingContext(ctx, md),
			&grpc.StreamDesc{ServerStreams: true, ClientStreams: true}, method, grpc.ForceCodec(codec{}))
		if err != nil {
			return err
		}
		// Client → upstream.
		sendErr := make(chan error, 1)
		go func() {
			for {
				f := &frame{}
				if err := ss.RecvMsg(f); err != nil {
					if errors.Is(err, io.EOF) {
						sendErr <- out.CloseSend()
					} else {
						sendErr <- err
					}
					return
				}
				if err := out.SendMsg(f); err != nil {
					sendErr <- nil // the upstream error comes back on RecvMsg
					return
				}
			}
		}()
		// Upstream → client.
		first := true
		for {
			f := &frame{}
			err := out.RecvMsg(f)
			if first {
				first = false
				if h, herr := out.Header(); herr == nil {
					delete(h, TierHeader)
					_ = ss.SendHeader(h)
				}
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				ss.SetTrailer(clean(out.Trailer()))
				return err
			}
			if err := ss.SendMsg(f); err != nil {
				return err
			}
		}
		ss.SetTrailer(clean(out.Trailer()))
		if err := <-sendErr; err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return nil
	}
	return []grpc.ServerOption{grpc.UnknownServiceHandler(handler), grpc.ForceServerCodec(codec{})}
}

func clean(md metadata.MD) metadata.MD {
	md = md.Copy()
	delete(md, TierHeader)
	return md
}
