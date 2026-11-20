// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/docker/spec"
)

// DefaultCallTimeout bounds every non-streaming Docker Engine call, so a
// hung daemon yields dependency-unavailable instead of pinning a goroutine
// (NFR-3). Image pull and log follow are exempt.
const DefaultCallTimeout = 30 * time.Second

// socketKey is the configuration key named by a socket error.
const socketKey = "providers.docker.socket"

// checkSocket accepts only the absolute path of a Unix socket on this host
// (CON-6): never a TCP address, an http URL or a relative path, so the
// Docker API is never reached over the network.
func checkSocket(socket string) error {
	switch {
	case socket == "":
		return fmt.Errorf("%s: is required", socketKey)
	case strings.Contains(socket, "://"):
		return fmt.Errorf("%s: must be the absolute path of the Docker Engine's Unix socket, not a URL; got %q",
			socketKey, socket)
	case !filepath.IsAbs(socket):
		return fmt.Errorf("%s: must be the absolute path of the Docker Engine's Unix socket; got %q", socketKey, socket)
	case strings.ContainsRune(socket, 0):
		return fmt.Errorf("%s: must not contain a NUL byte", socketKey)
	}
	return nil
}

// newClient returns an Engine API client for the Unix socket. moby/client
// negotiates the API version on its first request by default
// (client_options.go: WithAPIVersionNegotiation is deprecated as a no-op in
// v0.6.2), and no fixed version is set, so it works with any daemon at or
// above client.MinAPIVersion. It does not connect until the first call.
func newClient(socket string) (*client.Client, error) {
	if err := checkSocket(socket); err != nil {
		return nil, err
	}
	c, err := client.New(client.WithHost("unix://" + filepath.Clean(socket)))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", socketKey, err)
	}
	return c, nil
}

// callCtx bounds one non-streaming Engine call by the call timeout.
func (p *Provider) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, p.timeout)
}

// engineError turns an Engine call's error into the problem the caller
// sees. A not-found becomes notFound; the caller's own cancellation is
// passed through for the Dispatcher to treat as a departed caller; anything
// else is dependency-unavailable naming the provider, with the cause logged
// and never sent.
func (p *Provider) engineError(ctx context.Context, err error, what string, notFound *problem.Error) error {
	switch {
	case notFound != nil && cerrdefs.IsNotFound(err):
		return notFound
	case ctx.Err() != nil:
		return ctx.Err()
	}
	p.log.Warn("Docker Engine call failed", "provider", spec.Name, "call", what, "error", err.Error())
	return unavailable(describe(err, p.timeout))
}

func unavailable(detail string) *problem.Error {
	return problem.New(problem.DependencyUnavailable, detail).With("provider", spec.Name)
}

// describe is a client-safe summary of an Engine error.
func describe(err error, timeout time.Duration) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Sprintf("the Docker Engine did not answer within %s", timeout)
	case unreachable(err):
		return "the Docker Engine is not reachable"
	}
	return "the Docker Engine failed the request"
}

// unreachable reports whether err means the Engine could not be reached or
// did not answer in time, rather than that it answered with a failure.
func unreachable(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || client.IsErrConnectionFailed(err) || isDialError(err)
}

func isDialError(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) || errors.Is(err, os.ErrNotExist)
}

// reason is the preflight reason for a failed ping: what failed and where,
// with the innermost cause (such as "no such file or directory").
func reason(socket string, err error, timeout time.Duration) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Sprintf("the Docker Engine at %s did not answer a ping within %s", socket, timeout)
	}
	cause := err
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		cause = op.Err
	}
	return fmt.Sprintf("the Docker Engine is not reachable at %s: %v", socket, cause)
}
