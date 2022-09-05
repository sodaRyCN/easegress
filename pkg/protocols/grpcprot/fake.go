package grpcprot

import (
	"context"
	"google.golang.org/grpc"
)

type FakeServerStream struct {
	grpc.ServerStream
	ctx context.Context
}

func NewFakeServerStream(ctx context.Context) *FakeServerStream {
	return &FakeServerStream{ctx: ctx}
}

func (f *FakeServerStream) Context() context.Context {
	return f.ctx
}
