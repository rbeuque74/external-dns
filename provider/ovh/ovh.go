/*
Copyright 2020 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ovh

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/ovh/go-ovh/ovh"
	"github.com/patrickmn/go-cache"
	log "github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"

	"sigs.k8s.io/external-dns/endpoint"
	"sigs.k8s.io/external-dns/pkg/apis/externaldns"
	"sigs.k8s.io/external-dns/plan"
	"sigs.k8s.io/external-dns/provider"

	"go.uber.org/ratelimit"
)

const (
	ovhDefaultTTL = 0
	ovhCreate     = iota
	ovhDelete
	ovhUpdate
)

var (
	// ErrRecordToMutateNotFound when ApplyChange has to update/delete and didn't found the record in the existing zone (Change with no record ID)
	ErrRecordToMutateNotFound = errors.New("record to mutate not found in current zone")
	// ErrNoDryRun No dry run support for the moment
	ErrNoDryRun = errors.New("dry run not supported")
)

// OVHProvider is an implementation of Provider for OVH DNS.
type OVHProvider struct {
	provider.BaseProvider

	client ovhClient

	apiRateLimiter ratelimit.Limiter

	domainFilter endpoint.DomainFilter
	DryRun       bool

	// UseCache controls if the OVHProvider will cache records in memory, and serve them
	// without recontacting the OVHcloud API if the SOA of the domain zone hasn't changed.
	// Note that, when disabling cache, OVHcloud API has rate-limiting that will hit if
	// your refresh rate/number of records is too big, which might cause issue with the
	// provider.
	// Default value: true
	UseCache       bool
	lastRunRecords []ovhRecord
	lastRunZones   []string

	cacheInstance *cache.Cache
	dnsClient     dnsClient
}

type ovhClient interface {
	PostWithContext(context.Context, string, any, any) error
	PutWithContext(context.Context, string, any, any) error
	GetWithContext(context.Context, string, any) error
	DeleteWithContext(context.Context, string, any) error
}

type dnsClient interface {
	ExchangeContext(ctx context.Context, m *dns.Msg, a string) (*dns.Msg, time.Duration, error)
}

type ovhRecordFields struct {
	ovhRecordFieldUpdate
	FieldType string `json:"fieldType"`
}

type ovhRecordFieldUpdate struct {
	SubDomain string `json:"subDomain"`
	TTL       int64  `json:"ttl"`
	Target    string `json:"target"`
}

type ovhRecord struct {
	ovhRecordFields
	ID   uint64 `json:"id"`
	Zone string `json:"zone"`
}

type ovhChange struct {
	ovhRecord
	Action int
}

// NewOVHProvider initializes a new OVH DNS based Provider.
func NewOVHProvider(ctx context.Context, domainFilter endpoint.DomainFilter, endpoint string, apiRateLimit int, dryRun bool) (*OVHProvider, error) {
	client, err := ovh.NewEndpointClient(endpoint)
	if err != nil {
		return nil, err
	}

	client.UserAgent = "ExternalDNS/" + externaldns.Version

	// TODO: Add Dry Run support
	if dryRun {
		return nil, ErrNoDryRun
	}
	return &OVHProvider{
		client:         client,
		domainFilter:   domainFilter,
		apiRateLimiter: ratelimit.New(apiRateLimit),
		DryRun:         dryRun,
		cacheInstance:  cache.New(cache.NoExpiration, cache.NoExpiration),
		dnsClient:      new(dns.Client),
		UseCache:       true,
	}, nil
}

// Records returns the list of records in all relevant zones.
func (p *OVHProvider) Records(ctx context.Context) ([]*endpoint.Endpoint, error) {
	zones, records, err := p.zonesRecords(ctx)
	if err != nil {
		return nil, err
	}
	p.lastRunRecords = records
	p.lastRunZones = zones
	endpoints := ovhGroupByNameAndType(records)
	log.Infof("OVH: %d endpoints have been found", len(endpoints))
	return endpoints, nil
}

func planChangesByZoneName(zones []string, changes *plan.Changes) map[string]*plan.Changes {
	zoneNameIDMapper := provider.ZoneIDName{}
	for _, zone := range zones {
		zoneNameIDMapper.Add(zone, zone)
	}

	output := map[string]*plan.Changes{}
	for _, endpt := range changes.Delete {
		_, zoneName := zoneNameIDMapper.FindZone(endpt.DNSName)
		if _, ok := output[zoneName]; !ok {
			output[zoneName] = &plan.Changes{}
		}
		output[zoneName].Delete = append(output[zoneName].Delete, endpt)
	}
	for _, endpt := range changes.Create {
		_, zoneName := zoneNameIDMapper.FindZone(endpt.DNSName)
		if _, ok := output[zoneName]; !ok {
			output[zoneName] = &plan.Changes{}
		}
		output[zoneName].Create = append(output[zoneName].Create, endpt)
	}
	for _, endpt := range changes.UpdateOld {
		_, zoneName := zoneNameIDMapper.FindZone(endpt.DNSName)
		if _, ok := output[zoneName]; !ok {
			output[zoneName] = &plan.Changes{}
		}
		output[zoneName].UpdateOld = append(output[zoneName].UpdateOld, endpt)
	}
	for _, endpt := range changes.UpdateNew {
		_, zoneName := zoneNameIDMapper.FindZone(endpt.DNSName)
		if _, ok := output[zoneName]; !ok {
			output[zoneName] = &plan.Changes{}
		}
		output[zoneName].UpdateNew = append(output[zoneName].UpdateNew, endpt)
	}

	return output
}

func (p *OVHProvider) handleSingleZoneUpdate(ctx context.Context, zoneName string, existingRecords []ovhRecord, changes *plan.Changes) error {
	allChanges := []ovhChange{}
	var computedChanges []ovhChange

	computedChanges, existingRecords = newOvhChangeCreateDelete(ovhCreate, changes.Create, zoneName, existingRecords)
	allChanges = append(allChanges, computedChanges...)
	computedChanges, existingRecords = newOvhChangeCreateDelete(ovhDelete, changes.Delete, zoneName, existingRecords)
	allChanges = append(allChanges, computedChanges...)

	computedChanges = newOvhChangeUpdate(changes.UpdateOld, changes.UpdateNew, zoneName, existingRecords)
	allChanges = append(allChanges, computedChanges...)

	log.Infof("OVH: %q: %d changes will be done", zoneName, len(allChanges))

	eg, ctxErrGroup := errgroup.WithContext(ctx)
	for _, change := range allChanges {
		change := change
		eg.Go(func() error {
			return p.change(ctxErrGroup, change)
		})
	}

	err := eg.Wait()

	// do not refresh zone if errors: some records might haven't been processed yet, hence the zone will be in an inconsistent state
	// if modification of the zone was in error, invalidating the cache to make sure next run will start freshly
	if err == nil {
		err = p.refresh(ctx, zoneName)
	} else {
		p.invalidateCache(zoneName)
	}

	return err
}

// ApplyChanges applies a given set of changes in a given zone.
func (p *OVHProvider) ApplyChanges(ctx context.Context, changes *plan.Changes) (err error) {
	zones, records := p.lastRunZones, p.lastRunRecords
	defer func() {
		p.lastRunRecords = []ovhRecord{}
		p.lastRunZones = []string{}
	}()

	for _, change := range changes.Create {
		log.Debugf("OVH: changes CREATE %#v", *change)
	}
	for _, change := range changes.UpdateOld {
		log.Debugf("OVH: changes UPDATEOLD %#v", *change)
	}
	for _, change := range changes.UpdateNew {
		log.Debugf("OVH: changes UPDATENEW %#v", *change)
	}
	for _, change := range changes.Delete {
		log.Debugf("OVH: changes DELETE %#v", *change)
	}

	changesByZoneName := planChangesByZoneName(zones, changes)
	eg, ctx := errgroup.WithContext(ctx)

	for zoneName, changes := range changesByZoneName {
		eg.Go(func() error {
			return p.handleSingleZoneUpdate(ctx, zoneName, records, changes)
		})
	}

	if err := eg.Wait(); err != nil {
		return provider.NewSoftError(err)
	}

	return nil
}

func (p *OVHProvider) refresh(ctx context.Context, zone string) error {
	log.Debugf("OVH: Refresh %s zone", zone)

	// Zone has been altered so we invalidate the cache
	// so that the next run will reload it.
	p.invalidateCache(zone)

	p.apiRateLimiter.Take()
	if err := p.client.PostWithContext(ctx, fmt.Sprintf("/domain/zone/%s/refresh", url.PathEscape(zone)), nil, nil); err != nil {
		return provider.NewSoftError(err)
	}
	return nil
}

func (p *OVHProvider) change(ctx context.Context, change ovhChange) error {
	p.apiRateLimiter.Take()

	switch change.Action {
	case ovhCreate:
		log.Debugf("OVH: Add an entry to %s", change.String())
		return p.client.PostWithContext(ctx, fmt.Sprintf("/domain/zone/%s/record", url.PathEscape(change.Zone)), change.ovhRecordFields, nil)
	case ovhDelete:
		if change.ID == 0 {
			return ErrRecordToMutateNotFound
		}
		log.Debugf("OVH: Delete an entry to %s", change.String())
		return p.client.DeleteWithContext(ctx, fmt.Sprintf("/domain/zone/%s/record/%d", url.PathEscape(change.Zone), change.ID), nil)
	case ovhUpdate:
		if change.ID == 0 {
			return ErrRecordToMutateNotFound
		}
		log.Debugf("OVH: Update an entry to %s", change.String())
		return p.client.PutWithContext(ctx, fmt.Sprintf("/domain/zone/%s/record/%d", url.PathEscape(change.Zone), change.ID), change.ovhRecordFieldUpdate, nil)
	}

	return nil
}

func (p *OVHProvider) invalidateCache(zone string) {
	p.cacheInstance.Delete(zone + "#soa")
}

func (p *OVHProvider) zonesRecords(ctx context.Context) ([]string, []ovhRecord, error) {
	var allRecords []ovhRecord
	zones, err := p.zones(ctx)
	if err != nil {
		return nil, nil, provider.NewSoftError(err)
	}

	chRecords := make(chan []ovhRecord, len(zones))
	eg, ctx := errgroup.WithContext(ctx)
	for _, zone := range zones {
		zone := zone
		eg.Go(func() error { return p.records(ctx, &zone, chRecords, nil) })
	}
	if err := eg.Wait(); err != nil {
		return nil, nil, provider.NewSoftError(err)
	}
	close(chRecords)
	for records := range chRecords {
		allRecords = append(allRecords, records...)
	}
	return zones, allRecords, nil
}

func (p *OVHProvider) zones(ctx context.Context) ([]string, error) {
	zones := []string{}
	filteredZones := []string{}

	p.apiRateLimiter.Take()
	if err := p.client.GetWithContext(ctx, "/domain/zone", &zones); err != nil {
		return nil, err
	}

	for _, zoneName := range zones {
		if p.domainFilter.Match(zoneName) {
			filteredZones = append(filteredZones, zoneName)
		}
	}
	log.Infof("OVH: %d zones found", len(filteredZones))
	return filteredZones, nil
}

type ovhSoa struct {
	Server  string `json:"server"`
	Serial  uint32 `json:"serial"`
	records []ovhRecord
}

func (p *OVHProvider) records(ctx context.Context, zone *string, records chan<- []ovhRecord, filterRecords []*endpoint.Endpoint) error {
	var recordsIds []uint64
	ovhRecords := make([]ovhRecord, len(recordsIds))
	eg, ctxErrGroup := errgroup.WithContext(ctx)

	if p.UseCache {
		if cachedSoaItf, ok := p.cacheInstance.Get(*zone + "#soa"); ok {
			cachedSoa := cachedSoaItf.(ovhSoa)

			log.Debugf("OVH: Checking SOA against %v", cachedSoa.Serial)

			m := new(dns.Msg)
			m.SetQuestion(dns.Fqdn(*zone), dns.TypeSOA)
			in, _, err := p.dnsClient.ExchangeContext(ctx, m, strings.TrimSuffix(cachedSoa.Server, ".")+":53")
			if err == nil {
				if s, ok := in.Answer[0].(*dns.SOA); ok {
					if s.Serial == cachedSoa.Serial {
						log.Debugf("OVH: SOA from cache is valid")
						records <- cachedSoa.records
						return nil
					}
				}
			}

			p.invalidateCache(*zone)
		}
	}

	log.Debugf("OVH: Getting records for %s from API", *zone)

	p.apiRateLimiter.Take()
	var soa ovhSoa
	if p.UseCache {
		if err := p.client.GetWithContext(ctx, "/domain/zone/"+url.PathEscape(*zone)+"/soa", &soa); err != nil {
			return err
		}
	}

	if err := p.client.GetWithContext(ctx, fmt.Sprintf("/domain/zone/%s/record", url.PathEscape(*zone)), &recordsIds); err != nil {
		return err
	}
	chRecords := make(chan ovhRecord, len(recordsIds))
	for _, id := range recordsIds {
		id := id
		eg.Go(func() error { return p.record(ctxErrGroup, zone, id, chRecords) })
	}
	if err := eg.Wait(); err != nil {
		return err
	}
	close(chRecords)
	for record := range chRecords {
		ovhRecords = append(ovhRecords, record)
	}

	if p.UseCache {
		soa.records = ovhRecords
		_ = p.cacheInstance.Add(*zone+"#soa", soa, time.Hour)
	}

	records <- ovhRecords
	return nil
}

func (p *OVHProvider) record(ctx context.Context, zone *string, id uint64, records chan<- ovhRecord) error {
	record := ovhRecord{}

	log.Debugf("OVH: Getting record %d for %s", id, *zone)

	p.apiRateLimiter.Take()
	if err := p.client.GetWithContext(ctx, fmt.Sprintf("/domain/zone/%s/record/%d", url.PathEscape(*zone), id), &record); err != nil {
		return err
	}
	if provider.SupportedRecordType(record.FieldType) {
		log.Debugf("OVH: Record %d for %s is %+v", id, *zone, record)
		records <- record
	}
	return nil
}

func ovhGroupByNameAndType(records []ovhRecord) []*endpoint.Endpoint {
	endpoints := []*endpoint.Endpoint{}

	// group supported records by name and type
	groups := map[string][]ovhRecord{}

	for _, r := range records {
		groupBy := r.Zone + "//" + r.SubDomain + "//" + r.FieldType
		if _, ok := groups[groupBy]; !ok {
			groups[groupBy] = []ovhRecord{}
		}

		groups[groupBy] = append(groups[groupBy], r)
	}

	// create single endpoint with all the targets for each name/type
	for _, records := range groups {
		targets := []string{}
		for _, record := range records {
			targets = append(targets, record.Target)
		}
		endpoint := endpoint.NewEndpointWithTTL(
			strings.TrimPrefix(records[0].SubDomain+"."+records[0].Zone, "."),
			records[0].FieldType,
			endpoint.TTL(records[0].TTL),
			targets...,
		)
		endpoints = append(endpoints, endpoint)
	}

	return endpoints
}

func newOvhChangeCreateDelete(action int, endpoints []*endpoint.Endpoint, zone string, existingRecords []ovhRecord) ([]ovhChange, []ovhRecord) {
	ovhChanges := []ovhChange{}
	toDeleteIds := []int{}

	for _, e := range endpoints {
		for _, target := range e.Targets {
			change := ovhChange{
				Action: action,
				ovhRecord: ovhRecord{
					Zone: zone,
					ovhRecordFields: ovhRecordFields{
						FieldType: e.RecordType,
						ovhRecordFieldUpdate: ovhRecordFieldUpdate{
							SubDomain: strings.TrimSuffix(e.DNSName, "."+zone),
							TTL:       ovhDefaultTTL,
							Target:    target,
						},
					},
				},
			}
			if e.RecordTTL.IsConfigured() {
				change.TTL = int64(e.RecordTTL)
			}

			// The Zone might have multiple records with the same target. In order to avoid applying the action to the
			// same OVH record, we remove a record from the list when a match is found.
			if action == ovhDelete {
				for i, rec := range existingRecords {
					if rec.Zone == change.Zone && rec.SubDomain == change.SubDomain && rec.FieldType == change.FieldType && rec.Target == change.Target {
						change.ID = rec.ID
						toDeleteIds = append(toDeleteIds, i)
						break
					}
				}
			}

			ovhChanges = append(ovhChanges, change)
		}
	}

	if len(toDeleteIds) > 0 {
		// Copy the records because we need to mutate the list.
		existingRecords = slices.Clone(existingRecords)
		for _, id := range toDeleteIds {
			existingRecords = slices.Delete(existingRecords, id, id)
		}
	}

	return ovhChanges, existingRecords
}

func newOvhChangeUpdate(endpointsOld []*endpoint.Endpoint, endpointsNew []*endpoint.Endpoint, zone string, existingRecords []ovhRecord) []ovhChange {
	zoneNameIDMapper := provider.ZoneIDName{}
	zoneNameIDMapper.Add(zone, zone)

	oldEndpointByTypeAndName := map[string]*endpoint.Endpoint{}
	newEndpointByTypeAndName := map[string]*endpoint.Endpoint{}
	oldRecordsInZone := map[string][]ovhRecord{}

	for _, e := range endpointsOld {
		sub := strings.TrimSuffix(e.DNSName, "."+zone)
		oldEndpointByTypeAndName[e.RecordType+"//"+sub] = e
	}
	for _, e := range endpointsNew {
		sub := strings.TrimSuffix(e.DNSName, "."+zone)
		newEndpointByTypeAndName[e.RecordType+"//"+sub] = e
	}

	for id, _ := range oldEndpointByTypeAndName {
		for _, record := range existingRecords {
			if id == record.FieldType+"//"+record.SubDomain {
				oldRecordsInZone[id] = append(oldRecordsInZone[id], record)
			}
		}
	}

	changes := []ovhChange{}

	for id, _ := range oldEndpointByTypeAndName {
		oldRecords := slices.Clone(oldRecordsInZone[id])
		endpointsNew := newEndpointByTypeAndName[id]

		toInsertTarget := []string{}

		for _, target := range endpointsNew.Targets {
			var toDelete int = -1

			for i, record := range oldRecords {
				if target == record.Target {
					toDelete = i
					break
				}
			}

			if toDelete >= 0 {
				oldRecords = slices.Delete(oldRecords, toDelete, toDelete)
			} else {
				toInsertTarget = append(toInsertTarget, target)
			}
		}

		toInsertTargetToDelete := []int{}
		for i, target := range toInsertTarget {
			if len(oldRecords) == 0 {
				break
			}

			record := oldRecords[0]
			oldRecords = slices.Delete(oldRecords, 0, 0)
			record.Target = target

			if endpointsNew.RecordTTL.IsConfigured() {
				record.TTL = int64(endpointsNew.RecordTTL)
			} else {
				record.TTL = ovhDefaultTTL
			}
			changes = append(changes, ovhChange{
				Action:    ovhUpdate,
				ovhRecord: record,
			})
			toInsertTargetToDelete = append(toInsertTargetToDelete, i)
		}
		for _, i := range toInsertTargetToDelete {
			toInsertTarget = slices.Delete(toInsertTarget, i, i)
		}

		if len(toInsertTarget) > 0 {
			for _, target := range toInsertTarget {
				recordTTL := int64(ovhDefaultTTL)
				if endpointsNew.RecordTTL.IsConfigured() {
					recordTTL = int64(endpointsNew.RecordTTL)
				}

				changes = append(changes, ovhChange{
					Action: ovhCreate,
					ovhRecord: ovhRecord{
						Zone: zone,
						ovhRecordFields: ovhRecordFields{
							FieldType: endpointsNew.RecordType,
							ovhRecordFieldUpdate: ovhRecordFieldUpdate{
								SubDomain: strings.TrimSuffix(endpointsNew.DNSName, "."+zone),
								TTL:       recordTTL,
								Target:    target,
							},
						},
					},
				})
			}
		}

		if len(oldRecords) > 0 {
			for i := range oldRecords {
				changes = append(changes, ovhChange{
					Action:    ovhDelete,
					ovhRecord: oldRecords[i],
				})
			}
		}
	}

	return changes
}

func (c *ovhChange) String() string {
	if c.ID != 0 {
		return fmt.Sprintf("%s zone (ID : %d) : %s %d IN %s %s", c.Zone, c.ID, c.SubDomain, c.TTL, c.FieldType, c.Target)
	}
	return fmt.Sprintf("%s zone : %s %d IN %s %s", c.Zone, c.SubDomain, c.TTL, c.FieldType, c.Target)
}
