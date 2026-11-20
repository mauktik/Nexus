// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package services

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/mauktik/nexus/internal/apitypes"
	"github.com/mauktik/nexus/internal/platform"
	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/provider"
	"github.com/mauktik/nexus/internal/providers/services/spec"
	"github.com/mauktik/nexus/internal/svcctl"
)

var readCursorRE = regexp.MustCompile(spec.CursorPattern)

// readBindings binds list, get and logs (DF-1, DF-2).
func (p *Provider) readBindings() []provider.Binding {
	return []provider.Binding{
		provider.Sync(spec.List, p.list),
		provider.Sync(spec.Get, p.get),
		provider.Stream(spec.Logs, p.logs),
	}
}

// list lists every service the init system knows, filtered, sorted by name
// and paged (DF-1). svcctl.List runs the init system's two listings
// concurrently; controllable and protected come from one settings
// snapshot, taken once for the whole page.
func (p *Provider) list(ctx context.Context, in spec.ListInput) (apitypes.List[spec.Service], error) {
	var none apitypes.List[spec.Service]
	if err := readCheckList(in); err != nil {
		return none, err
	}
	after, err := readDecodeCursor(in.Cursor)
	if err != nil {
		return none, err
	}
	snap := p.snapshot()
	entries, err := svcctl.List(ctx, p.plat.Init, p.svcctlOptions())
	if err != nil {
		return none, readFailure(ctx, err)
	}
	items := make([]spec.Service, 0, len(entries))
	for _, e := range entries {
		props := readProperties(snap, e, "")
		if !readMatches(in, props) {
			continue
		}
		items = append(items, apitypes.NewResource(p.host, spec.Name, spec.ResourceType, e.Name, props))
	}
	slices.SortFunc(items, func(a, b spec.Service) int { return strings.Compare(a.Name, b.Name) })
	limit := in.Limit
	if limit == 0 {
		limit = spec.DefaultLimit
	}
	start, found := slices.BinarySearchFunc(items, after, func(s spec.Service, name string) int {
		return strings.Compare(s.Name, name)
	})
	if found {
		start++
	}
	end := min(start+limit, len(items))
	out := apitypes.List[spec.Service]{Items: items[start:end]}
	if end < len(items) {
		out.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(items[end-1].Name))
	}
	return out, nil
}

// readCheckList refuses, itself, every list input the schema refuses, so a
// direct call cannot pass one.
func readCheckList(in spec.ListInput) error {
	switch {
	case in.State != "" && !slices.Contains(spec.States, in.State):
		return readInvalid("/state", "must be one of "+strings.Join(spec.States, ", "))
	case in.Enabled != "" && !slices.Contains(spec.EnabledValues, in.Enabled):
		return readInvalid("/enabled", "must be one of "+strings.Join(spec.EnabledValues, ", "))
	case in.Controllable != "" && in.Controllable != "true" && in.Controllable != "false":
		return readInvalid("/controllable", "must be true or false")
	case in.Limit < 0 || in.Limit > spec.MaxLimit:
		return readInvalid("/limit", fmt.Sprintf("must be between 1 and %d", spec.MaxLimit))
	}
	return nil
}

// readMatches reports whether a service passes the list's filters.
func readMatches(in spec.ListInput, props spec.ServiceProperties) bool {
	switch {
	case in.State != "" && props.State != in.State:
		return false
	case in.Enabled != "" && props.Enabled != in.Enabled:
		return false
	case in.Controllable != "" && fmt.Sprint(props.Controllable) != in.Controllable:
		return false
	}
	return true
}

// readProperties are e's list properties, with controllable and protected
// from snap. canonical is e's unit Id without .service ("" when unknown).
func readProperties(snap *snapshot, e svcctl.Entry, canonical string) spec.ServiceProperties {
	props := spec.ServiceProperties{
		State: e.State, RawState: e.RawState, Enabled: e.Enabled, EnabledRaw: e.EnabledRaw,
		Runlevels: slices.Clone(e.Runlevels),
	}
	if e.DescriptionKnown {
		d := e.Description
		props.Description = &d
	}
	props.Protected, props.ProtectedReason = snap.prot.protected(e.Name, canonical)
	props.Controllable = snap.controllable(e.Name, canonical)
	return props
}

// readDecodeCursor is the name a list cursor carries: the unpadded
// base64url of the last name of the previous page, as nginx's.
func readDecodeCursor(c string) (string, error) {
	if c == "" {
		return "", nil
	}
	name, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil || len(c) > spec.MaxCursorLength || !readCursorRE.MatchString(c) || len(name) == 0 {
		return "", readInvalid("/cursor", "is not a cursor this server issued")
	}
	return string(name), nil
}

// get returns one service with the list's properties and what Describe
// reads (DF-1 step 5). A field the init system does not report is null with
// its reason in unavailable, and logs says whether logs can be read.
func (p *Provider) get(ctx context.Context, in spec.NameInput) (spec.ServiceDetail, error) {
	var none spec.ServiceDetail
	if err := readCheckName(in.Name); err != nil {
		return none, err
	}
	snap := p.snapshot()
	d, err := svcctl.Describe(ctx, p.plat.Init, in.Name, p.svcctlOptions())
	if err != nil {
		return none, readFailure(ctx, err)
	}
	if !d.Known {
		return none, readNotFound(in.Name)
	}
	details := spec.ServiceDetails{
		ServiceProperties: readProperties(snap, d.Entry, d.CanonicalName),
		Since:             d.Since,
		Restarts:          d.Restarts,
		MemoryBytes:       d.MemoryBytes,
		Logs:              p.logsInfo(in.Name),
		Unavailable:       map[string]string{},
	}
	reported := func(key string) bool {
		_, missing := d.Unavailable[key]
		return !missing
	}
	if d.MainPID > 0 {
		pid := d.MainPID
		details.MainPID = &pid
	}
	if reported("subState") {
		details.SubState = &d.SubState
	}
	if reported("result") {
		details.Result = &d.Result
	}
	if d.UnitFilePath != "" {
		details.UnitFilePath = &d.UnitFilePath
	}
	if d.ScriptPath != "" {
		details.ScriptPath = &d.ScriptPath
	}
	if d.CanonicalName != "" {
		details.CanonicalName = &d.CanonicalName
	}
	// Every null field has a reason, and only a null field has one.
	for key, null := range map[string]bool{
		"description": details.Description == nil, "since": details.Since == nil, "mainPid": details.MainPID == nil,
		"subState": details.SubState == nil, "result": details.Result == nil, "restarts": details.Restarts == nil,
		"memoryBytes": details.MemoryBytes == nil, "unitFilePath": details.UnitFilePath == nil,
		"scriptPath": details.ScriptPath == nil, "canonicalName": details.CanonicalName == nil,
	} {
		if !null {
			continue
		}
		reason := d.Unavailable[key]
		if reason == "" {
			reason = readUnreported(p.plat.Init, key)
		}
		details.Unavailable[key] = reason
	}
	if len(details.Unavailable) == 0 {
		details.Unavailable = nil
	}
	return apitypes.NewResource(p.host, spec.Name, spec.ResourceType, in.Name, details), nil
}

// readUnreported is the reason for a null field svcctl gave none for.
func readUnreported(init platform.InitSystem, key string) string {
	if key == "description" {
		if init == platform.OpenRC {
			return "the init script declares no static description"
		}
		return "the unit has no description"
	}
	return "not reported"
}

// logsInfo says whether name's logs can be read, and from where: the
// journal on systemd, or the log files an OpenRC service declares
// (svcctl.OpenRCLogFiles, which runs nothing).
func (p *Provider) logsInfo(name string) spec.LogsInfo {
	if p.plat.Init != platform.OpenRC {
		return spec.LogsInfo{Available: true, Source: logSource(p.plat.Init)}
	}
	lf, err := svcctl.OpenRCLogFiles(p.etcRoot, name)
	switch {
	case err != nil:
		return spec.LogsInfo{Source: spec.LogSourceUnavailable,
			Reason: "the service's log declarations cannot be read: " + readDetail(err)}
	case lf.Output == "" && lf.Error == "":
		return spec.LogsInfo{Source: spec.LogSourceUnavailable, Reason: lf.Reason}
	}
	return spec.LogsInfo{Available: true, Source: spec.LogSourceOpenRCFiles}
}

// readCheckName refuses a name get, logs or control cannot address, before
// anything runs.
func readCheckName(name string) error {
	switch {
	case !serviceName.MatchString(name):
		return readInvalid("/name", "must match "+spec.ServiceNamePattern)
	case strings.HasSuffix(name, ".service"):
		return readInvalid("/name", "is the service name without .service")
	}
	return nil
}

// readFailure is the answer to a failed read of the init system: the
// caller's own cancellation as it is, otherwise 503 dependency-unavailable
// naming the tool and its first line of output (svcctl's error says both).
func readFailure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return err
	}
	return problem.New(problem.DependencyUnavailable, readDetail(err)).With("provider", spec.Name)
}

// readDetail is a svcctl error without its package prefix.
func readDetail(err error) string {
	return strings.TrimPrefix(err.Error(), "svcctl: ")
}

func readNotFound(name string) *problem.Error {
	return problem.New(problem.NotFound, fmt.Sprintf("the init system does not know service %s", name))
}

func readInvalid(path, msg string) *problem.Error {
	return problem.New(problem.ValidationFailed, strings.TrimPrefix(path, "/")+" "+msg).
		With("errors", []problem.FieldError{{Path: path, Message: msg}})
}
