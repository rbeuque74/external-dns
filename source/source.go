/*
Copyright 2017 The Kubernetes Authors.

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

package source

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/netip"
	"reflect"
	"strconv"
	"strings"
	"text/template"
	"time"
	"unicode"

	log "github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"sigs.k8s.io/external-dns/endpoint"
)

const (
	// The annotation used for figuring out which controller is responsible
	controllerAnnotationKey = "external-dns.alpha.kubernetes.io/controller"
	// The annotation used for defining the desired hostname
	hostnameAnnotationKey = "external-dns.alpha.kubernetes.io/hostname"
	// The annotation used for specifying whether the public or private interface address is used
	accessAnnotationKey = "external-dns.alpha.kubernetes.io/access"
	// The annotation used for specifying the type of endpoints to use for headless services
	endpointsTypeAnnotationKey = "external-dns.alpha.kubernetes.io/endpoints-type"
	// The annotation used for defining the desired ingress/service target
	targetAnnotationKey = "external-dns.alpha.kubernetes.io/target"
	// The annotation used for defining the desired DNS record TTL
	ttlAnnotationKey = "external-dns.alpha.kubernetes.io/ttl"
	// The annotation used for switching to the alias record types e. g. AWS Alias records instead of a normal CNAME
	aliasAnnotationKey = "external-dns.alpha.kubernetes.io/alias"
	// The annotation used to determine the source of hostnames for ingresses.  This is an optional field - all
	// available hostname sources are used if not specified.
	ingressHostnameSourceKey = "external-dns.alpha.kubernetes.io/ingress-hostname-source"
	// The value of the controller annotation so that we feel responsible
	controllerAnnotationValue = "dns-controller"
	// The annotation used for defining the desired hostname
	internalHostnameAnnotationKey = "external-dns.alpha.kubernetes.io/internal-hostname"
)

const (
	EndpointsTypeNodeExternalIP = "NodeExternalIP"
	EndpointsTypeHostIP         = "HostIP"
)

// Provider-specific annotations
const (
	// The annotation used for determining if traffic will go through Cloudflare
	CloudflareProxiedKey        = "external-dns.alpha.kubernetes.io/cloudflare-proxied"
	CloudflareCustomHostnameKey = "external-dns.alpha.kubernetes.io/cloudflare-custom-hostname"
	CloudflareRegionKey         = "external-dns.alpha.kubernetes.io/cloudflare-region-key"

	SetIdentifierKey = "external-dns.alpha.kubernetes.io/set-identifier"
)

const (
	ttlMinimum = 1
	ttlMaximum = math.MaxInt32
)

// Source defines the interface Endpoint sources should implement.
type Source interface {
	Endpoints(ctx context.Context) ([]*endpoint.Endpoint, error)
	// AddEventHandler adds an event handler that should be triggered if something in source changes
	AddEventHandler(context.Context, func())
}

func getTTLFromAnnotations(ants map[string]string, resource string) endpoint.TTL {
	ttlNotConfigured := endpoint.TTL(0)
	ttlAnnotation, ok := ants[ttlAnnotationKey]
	if !ok {
		return ttlNotConfigured
	}
	ttlValue, err := parseTTL(ttlAnnotation)
	if err != nil {
		log.Warnf("%s: \"%v\" is not a valid TTL value: %v", resource, ttlAnnotation, err)
		return ttlNotConfigured
	}
	if ttlValue < ttlMinimum || ttlValue > ttlMaximum {
		log.Warnf("TTL value %q must be between [%d, %d]", ttlValue, ttlMinimum, ttlMaximum)
		return ttlNotConfigured
	}
	return endpoint.TTL(ttlValue)
}

// parseTTL parses TTL from string, returning duration in seconds.
// parseTTL supports both integers like "600" and durations based
// on Go Duration like "10m", hence "600" and "10m" represent the same value.
//
// Note: for durations like "1.5s" the fraction is omitted (resulting in 1 second
// for the example).
func parseTTL(s string) (ttlSeconds int64, err error) {
	ttlDuration, errDuration := time.ParseDuration(s)
	if errDuration != nil {
		ttlInt, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, errDuration
		}
		return ttlInt, nil
	}

	return int64(ttlDuration.Seconds()), nil
}

type kubeObject interface {
	runtime.Object
	metav1.Object
}

func execTemplate(tmpl *template.Template, obj kubeObject) (hostnames []string, err error) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, obj); err != nil {
		kind := obj.GetObjectKind().GroupVersionKind().Kind
		return nil, fmt.Errorf("failed to apply template on %s %s/%s: %w", kind, obj.GetNamespace(), obj.GetName(), err)
	}
	for _, name := range strings.Split(buf.String(), ",") {
		name = strings.TrimFunc(name, unicode.IsSpace)
		name = strings.TrimSuffix(name, ".")
		hostnames = append(hostnames, name)
	}
	return hostnames, nil
}

func getHostnamesFromAnnotations(annotations map[string]string) []string {
	hostnameAnnotation, exists := annotations[hostnameAnnotationKey]
	if !exists {
		return nil
	}
	return splitHostnameAnnotation(hostnameAnnotation)
}

func getAccessFromAnnotations(annotations map[string]string) string {
	return annotations[accessAnnotationKey]
}

func getEndpointsTypeFromAnnotations(annotations map[string]string) string {
	return annotations[endpointsTypeAnnotationKey]
}

func getInternalHostnamesFromAnnotations(ants map[string]string) []string {
	internalHostnameAnnotation, ok := ants[internalHostnameAnnotationKey]
	if !ok {
		return nil
	}
	return splitHostnameAnnotation(internalHostnameAnnotation)
}

func splitHostnameAnnotation(annotation string) []string {
	return strings.Split(strings.ReplaceAll(annotation, " ", ""), ",")
}

func getAliasFromAnnotations(ants map[string]string) bool {
	aliasAnnotation, exists := ants[aliasAnnotationKey]
	return exists && aliasAnnotation == "true"
}

func getProviderSpecificAnnotations(ants map[string]string) (endpoint.ProviderSpecific, string) {
	providerSpecificAnnotations := endpoint.ProviderSpecific{}

	if v, exists := ants[CloudflareProxiedKey]; exists {
		providerSpecificAnnotations = append(providerSpecificAnnotations, endpoint.ProviderSpecificProperty{
			Name:  CloudflareProxiedKey,
			Value: v,
		})
	}
	if v, exists := ants[CloudflareCustomHostnameKey]; exists {
		providerSpecificAnnotations = append(providerSpecificAnnotations, endpoint.ProviderSpecificProperty{
			Name:  CloudflareCustomHostnameKey,
			Value: v,
		})
	}
	if v, exists := ants[CloudflareRegionKey]; exists {
		providerSpecificAnnotations = append(providerSpecificAnnotations, endpoint.ProviderSpecificProperty{
			Name:  CloudflareRegionKey,
			Value: v,
		})
	}
	if getAliasFromAnnotations(ants) {
		providerSpecificAnnotations = append(providerSpecificAnnotations, endpoint.ProviderSpecificProperty{
			Name:  "alias",
			Value: "true",
		})
	}
	setIdentifier := ""
	for k, v := range ants {
		if k == SetIdentifierKey {
			setIdentifier = v
		} else if strings.HasPrefix(k, "external-dns.alpha.kubernetes.io/aws-") {
			attr := strings.TrimPrefix(k, "external-dns.alpha.kubernetes.io/aws-")
			providerSpecificAnnotations = append(providerSpecificAnnotations, endpoint.ProviderSpecificProperty{
				Name:  fmt.Sprintf("aws/%s", attr),
				Value: v,
			})
		} else if strings.HasPrefix(k, "external-dns.alpha.kubernetes.io/scw-") {
			attr := strings.TrimPrefix(k, "external-dns.alpha.kubernetes.io/scw-")
			providerSpecificAnnotations = append(providerSpecificAnnotations, endpoint.ProviderSpecificProperty{
				Name:  fmt.Sprintf("scw/%s", attr),
				Value: v,
			})
		} else if strings.HasPrefix(k, "external-dns.alpha.kubernetes.io/ibmcloud-") {
			attr := strings.TrimPrefix(k, "external-dns.alpha.kubernetes.io/ibmcloud-")
			providerSpecificAnnotations = append(providerSpecificAnnotations, endpoint.ProviderSpecificProperty{
				Name:  fmt.Sprintf("ibmcloud-%s", attr),
				Value: v,
			})
		} else if strings.HasPrefix(k, "external-dns.alpha.kubernetes.io/webhook-") {
			// Support for wildcard annotations for webhook providers
			attr := strings.TrimPrefix(k, "external-dns.alpha.kubernetes.io/webhook-")
			providerSpecificAnnotations = append(providerSpecificAnnotations, endpoint.ProviderSpecificProperty{
				Name:  fmt.Sprintf("webhook/%s", attr),
				Value: v,
			})
		}
	}
	return providerSpecificAnnotations, setIdentifier
}

// getTargetsFromTargetAnnotation gets endpoints from optional "target" annotation.
// Returns empty endpoints array if none are found.
func getTargetsFromTargetAnnotation(annotations map[string]string) endpoint.Targets {
	var targets endpoint.Targets

	// Get the desired hostname of the ingress from the annotation.
	targetAnnotation, exists := annotations[targetAnnotationKey]
	if exists && targetAnnotation != "" {
		// splits the hostname annotation and removes the trailing periods
		targetsList := strings.Split(strings.ReplaceAll(targetAnnotation, " ", ""), ",")
		for _, targetHostname := range targetsList {
			targetHostname = strings.TrimSuffix(targetHostname, ".")
			targets = append(targets, targetHostname)
		}
	}
	return targets
}

// suitableType returns the DNS resource record type suitable for the target.
// In this case type A/AAAA for IPs and type CNAME for everything else.
func suitableType(target string) string {
	netIP, err := netip.ParseAddr(target)
	if err == nil && netIP.Is4() {
		return endpoint.RecordTypeA
	} else if err == nil && netIP.Is6() {
		return endpoint.RecordTypeAAAA
	}
	return endpoint.RecordTypeCNAME
}

// endpointsForHostname returns the endpoint objects for each host-target combination.
func endpointsForHostname(hostname string, targets endpoint.Targets, ttl endpoint.TTL, providerSpecific endpoint.ProviderSpecific, setIdentifier string, resource string) []*endpoint.Endpoint {
	var endpoints []*endpoint.Endpoint

	var aTargets endpoint.Targets
	var aaaaTargets endpoint.Targets
	var cnameTargets endpoint.Targets

	for _, t := range targets {
		switch suitableType(t) {
		case endpoint.RecordTypeA:
			aTargets = append(aTargets, t)
		case endpoint.RecordTypeAAAA:
			aaaaTargets = append(aaaaTargets, t)
		default:
			cnameTargets = append(cnameTargets, t)
		}
	}

	if len(aTargets) > 0 {
		epA := endpoint.NewEndpointWithTTL(hostname, endpoint.RecordTypeA, ttl, aTargets...)
		if epA != nil {
			epA.ProviderSpecific = providerSpecific
			epA.SetIdentifier = setIdentifier
			if resource != "" {
				epA.Labels[endpoint.ResourceLabelKey] = resource
			}
			endpoints = append(endpoints, epA)
		}
	}

	if len(aaaaTargets) > 0 {
		epAAAA := endpoint.NewEndpointWithTTL(hostname, endpoint.RecordTypeAAAA, ttl, aaaaTargets...)
		if epAAAA != nil {
			epAAAA.ProviderSpecific = providerSpecific
			epAAAA.SetIdentifier = setIdentifier
			if resource != "" {
				epAAAA.Labels[endpoint.ResourceLabelKey] = resource
			}
			endpoints = append(endpoints, epAAAA)
		}
	}

	if len(cnameTargets) > 0 {
		epCNAME := endpoint.NewEndpointWithTTL(hostname, endpoint.RecordTypeCNAME, ttl, cnameTargets...)
		if epCNAME != nil {
			epCNAME.ProviderSpecific = providerSpecific
			epCNAME.SetIdentifier = setIdentifier
			if resource != "" {
				epCNAME.Labels[endpoint.ResourceLabelKey] = resource
			}
			endpoints = append(endpoints, epCNAME)
		}
	}

	return endpoints
}

func getLabelSelector(annotationFilter string) (labels.Selector, error) {
	labelSelector, err := metav1.ParseToLabelSelector(annotationFilter)
	if err != nil {
		return nil, err
	}
	return metav1.LabelSelectorAsSelector(labelSelector)
}

func matchLabelSelector(selector labels.Selector, srcAnnotations map[string]string) bool {
	return selector.Matches(labels.Set(srcAnnotations))
}

type eventHandlerFunc func()

func (fn eventHandlerFunc) OnAdd(obj interface{}, isInInitialList bool) { fn() }
func (fn eventHandlerFunc) OnUpdate(oldObj, newObj interface{})         { fn() }
func (fn eventHandlerFunc) OnDelete(obj interface{})                    { fn() }

type informerFactory interface {
	WaitForCacheSync(stopCh <-chan struct{}) map[reflect.Type]bool
}

func waitForCacheSync(ctx context.Context, factory informerFactory) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for typ, done := range factory.WaitForCacheSync(ctx.Done()) {
		if !done {
			select {
			case <-ctx.Done():
				return fmt.Errorf("failed to sync %v: %v", typ, ctx.Err())
			default:
				return fmt.Errorf("failed to sync %v", typ)
			}
		}
	}
	return nil
}

type dynamicInformerFactory interface {
	WaitForCacheSync(stopCh <-chan struct{}) map[schema.GroupVersionResource]bool
}

func waitForDynamicCacheSync(ctx context.Context, factory dynamicInformerFactory) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	for typ, done := range factory.WaitForCacheSync(ctx.Done()) {
		if !done {
			select {
			case <-ctx.Done():
				return fmt.Errorf("failed to sync %v: %v", typ, ctx.Err())
			default:
				return fmt.Errorf("failed to sync %v", typ)
			}
		}
	}
	return nil
}
