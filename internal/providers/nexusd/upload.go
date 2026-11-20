// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package nexusd

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strconv"

	"github.com/mauktik/nexus/internal/audit"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/nexusd/spec"
)

// stagingOf returns the staging store, or an error when UseDaemon was not
// called.
func (p *Provider) stagingOf() (*staging, error) {
	s := p.staging.Load()
	if s == nil {
		return nil, errors.New("nexusd: no staging: UseDaemon was not called")
	}
	return s, nil
}

// openStaging creates <StateDir>/staging and sweeps it (A-9). A failure is
// logged: the upload then fails when it cannot create its directory.
func (p *Provider) openStaging(d provider.Daemon) *staging {
	s := newStaging(filepath.Join(d.StateDir(), stagingDirName), p.now, p.afterFunc)
	if err := s.open(); err != nil {
		slog.Error("nexusd: cannot open the staging directory", "dir", s.dir, "error", err.Error())
	}
	return s
}

// uploadBinding binds nexusd.uploads.add: the body cap is
// providers.nexusd.update.maxUploadBytes, Admit reserves a staging slot and
// Receive stores the body (F4).
func (p *Provider) uploadBinding() provider.Binding {
	return provider.Upload(spec.Upload, provider.UploadSpec[spec.UploadInput, spec.UploadResult]{
		MaxBytes: func() int64 { return p.current().cfg.Update.MaxUploadBytes },
		Admit:    p.admitUpload,
		Receive:  p.receiveUpload,
	})
}

// admitUpload refuses without self-management, holding nothing, and
// otherwise returns the staging reservation as it is: a slot and its
// release, or conflict when maxStaged are counted (A-51).
func (p *Provider) admitUpload(context.Context, spec.UploadInput) (func(), error) {
	if err := p.requireSelfManagement(); err != nil {
		return nil, err
	}
	s, err := p.stagingOf()
	if err != nil {
		return nil, err
	}
	return s.reserve()
}

// receiveUpload stores the body and adds the facts uploadId, sha256 and
// sizeBytes to the stored record (FR-16).
func (p *Provider) receiveUpload(ctx context.Context, in spec.UploadInput, b provider.UploadBody) (spec.UploadResult, error) {
	s, err := p.stagingOf()
	if err != nil {
		return spec.UploadResult{}, err
	}
	m, err := s.receive(ctx, in.Signature, b.Reader)
	if err != nil {
		return spec.UploadResult{}, err
	}
	audit.AddFact(ctx, "uploadId", m.UploadID)
	audit.AddFact(ctx, "sha256", m.SHA256)
	audit.AddFact(ctx, "sizeBytes", strconv.FormatInt(m.SizeBytes, 10))
	return spec.UploadResult{UploadID: m.UploadID, SHA256: m.SHA256, SizeBytes: m.SizeBytes, ExpiresAt: m.Expires}, nil
}
