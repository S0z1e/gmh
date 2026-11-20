package rpc

import "io"

func handleStdoutBrokenPipe(io.Writer) func() { return func() {} }
