// Copyright 2021 Gravitational, Inc
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package desktop

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-ldap/ldap/v3"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"
	"github.com/gravitational/trace"
)

// computerAttributes are the attributes we fetch when discovering
// Windows hosts via LDAP
// see: https://docs.microsoft.com/en-us/windows/win32/adschema/c-computer#windows-server-2012-attributes
var computerAttribtes = []string{
	attrName,
	attrDNSHostName,
	attrObjectGUID,
	attrOS,
	attrOSVersion,
	attrPrimaryGroupID,
}

const (
	// computerClass is the object class for computers in Active Directory
	computerClass = "computer"
	// containerClass is the object class for containers in Active Directory
	containerClass = "container"
	// gmsaClass is the object class for group managed service accounts in Active Directory.
	gmsaClass = "msDS-GroupManagedServiceAccount"

	// See: https://docs.microsoft.com/en-US/windows/security/identity-protection/access-control/security-identifiers
	writableDomainControllerGroupID = "516"
	readOnlyDomainControllerGroupID = "521"

	attrName           = "name"
	attrDNSHostName    = "dNSHostName" // unusual capitalization is correct
	attrObjectGUID     = "objectGUID"
	attrOS             = "operatingSystem"
	attrOSVersion      = "operatingSystemVersion"
	attrPrimaryGroupID = "primaryGroupID"
)

// startDesktopDiscovery starts fetching desktops from LDAP, periodically
// registering and unregistering them as necessary.
func (s *WindowsService) startDesktopDiscovery() error {
	reconciler, err := services.NewReconciler(services.ReconcilerConfig{
		// Use a matcher that matches all resources, since our desktops are
		// pre-filtered by nature of using an LDAP search with filters.
		Matcher: func(r types.ResourceWithLabels) bool { return true },

		GetCurrentResources: func() types.ResourcesWithLabels { return s.lastDiscoveryResults },
		GetNewResources:     s.getDesktopsFromLDAP,
		OnCreate:            s.upsertDesktop,
		OnUpdate:            s.upsertDesktop,
		OnDelete:            s.deleteDesktop,
		Log:                 s.cfg.Log,
	})
	if err != nil {
		return trace.Wrap(err)
	}

	go func() {
		// reconcile once before starting the ticker, so that desktops show up immediately
		// (we still have a small delay to give the LDAP client time to initialize)
		time.Sleep(15 * time.Second)
		if err := reconciler.Reconcile(s.closeCtx); err != nil && err != context.Canceled {
			s.cfg.Log.Errorf("desktop reconciliation failed: %v", err)
		}

		// TODO(zmb3): consider making the discovery period configurable
		// (it's currently hard coded to 5 minutes in order to match DB access discovery behavior)
		t := s.cfg.Clock.NewTicker(5 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-s.closeCtx.Done():
				return
			case <-t.Chan():
				if err := reconciler.Reconcile(s.closeCtx); err != nil && err != context.Canceled {
					s.cfg.Log.Errorf("desktop reconciliation failed: %v", err)
				}
			}
		}
	}()

	return nil
}

func (s *WindowsService) ldapSearchFilter() string {
	var filters []string
	filters = append(filters, fmt.Sprintf("(objectClass=%s)", computerClass))
	filters = append(filters, fmt.Sprintf("(!(objectClass=%s))", gmsaClass))
	filters = append(filters, s.cfg.DiscoveryLDAPFilters...)

	return combineLDAPFilters(filters)
}

// getDesktopsFromLDAP discovers Windows hosts via LDAP
func (s *WindowsService) getDesktopsFromLDAP() types.ResourcesWithLabels {
	if !s.ldapReady() {
		s.cfg.Log.Warn("skipping desktop discovery: LDAP not yet initialized")
		return nil
	}

	filter := s.ldapSearchFilter()
	s.cfg.Log.Debugf("searching for desktops with LDAP filter %v", filter)

	entries, err := s.lc.readWithFilter(s.cfg.DiscoveryBaseDN, filter, computerAttribtes)
	if trace.IsConnectionProblem(err) {
		// If the connection was broken, re-initialize the LDAP client so that it's
		// ready for the next reconcile loop. Return the last known set of desktops
		// in this case, so that the reconciler doesn't delete the desktops it already
		// knows about.
		s.cfg.Log.Info("LDAP connection error when searching for desktops, reinitializing client")
		if err := s.initializeLDAP(); err != nil {
			s.cfg.Log.Errorf("failed to reinitialize LDAP client, will retry on next reconcile: %v", err)
		}
		return s.lastDiscoveryResults
	} else if err != nil {
		s.cfg.Log.Warnf("could not discover Windows Desktops: %v", err)
		return nil
	}

	s.cfg.Log.Debugf("discovered %d Windows Desktops", len(entries))

	var result types.ResourcesWithLabels
	for _, entry := range entries {
		desktop, err := s.ldapEntryToWindowsDesktop(s.closeCtx, entry, s.cfg.HostLabelsFn)
		if err != nil {
			s.cfg.Log.Warnf("could not create Windows Desktop from LDAP entry: %v", err)
			continue
		}
		result = append(result, desktop)
	}

	// capture the result, which will be used on the next reconcile loop
	s.lastDiscoveryResults = result

	return result
}

func (s *WindowsService) upsertDesktop(ctx context.Context, r types.ResourceWithLabels) error {
	d, ok := r.(types.WindowsDesktop)
	if !ok {
		return trace.Errorf("upsert: expected a WindowsDesktop, got %T", r)
	}
	return s.cfg.AuthClient.UpsertWindowsDesktop(ctx, d)
}

func (s *WindowsService) deleteDesktop(ctx context.Context, r types.ResourceWithLabels) error {
	d, ok := r.(types.WindowsDesktop)
	if !ok {
		return trace.Errorf("delete: expected a WindowsDesktop, got %T", r)
	}
	return s.cfg.AuthClient.DeleteWindowsDesktop(ctx, d.GetHostID(), d.GetName())
}

func applyLabelsFromLDAP(entry *ldap.Entry, labels map[string]string) {
	labels["teleport.dev/dns_host_name"] = entry.GetAttributeValue(attrDNSHostName)
	labels["teleport.dev/computer_name"] = entry.GetAttributeValue(attrName)
	labels["teleport.dev/os"] = entry.GetAttributeValue(attrOS)
	labels["teleport.dev/os_version"] = entry.GetAttributeValue(attrOSVersion)
	labels[types.OriginLabel] = types.OriginDynamic
	switch entry.GetAttributeValue(attrPrimaryGroupID) {
	case writableDomainControllerGroupID, readOnlyDomainControllerGroupID:
		labels["teleport.dev/is_domain_controller"] = "true"
	}
}

// ldapEntryToWindowsDesktop generates the Windows Desktop resource
// from an LDAP search result
func (s *WindowsService) ldapEntryToWindowsDesktop(ctx context.Context, entry *ldap.Entry, getHostLabels func(string) map[string]string) (types.ResourceWithLabels, error) {
	hostname := entry.GetAttributeValue(attrDNSHostName)
	labels := getHostLabels(hostname)
	labels["teleport.dev/windows_domain"] = s.cfg.Domain
	applyLabelsFromLDAP(entry, labels)

	addrs, err := s.dnsResolver.LookupHost(ctx, hostname)
	if err != nil || len(addrs) == 0 {
		return nil, trace.WrapWithMessage(err, "couldn't resolve %q", hostname)
	}

	s.cfg.Log.Debugf("resolved %v => %v", hostname, addrs)
	addr, err := utils.ParseHostPortAddr(addrs[0], defaults.RDPListenPort)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	desktop, err := types.NewWindowsDesktopV3(
		// ensure no '.' in name, because we use SNI to route to the right
		// desktop, and our cert is valid for *.desktop.teleport.cluster.local
		strings.ReplaceAll(hostname, ".", "-"),
		labels,
		types.WindowsDesktopSpecV3{
			Addr:   addr.String(),
			Domain: s.cfg.Domain,
			HostID: s.cfg.Heartbeat.HostUUID,
		},
	)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	desktop.SetExpiry(s.cfg.Clock.Now().UTC().Add(apidefaults.ServerAnnounceTTL))
	return desktop, nil
}
