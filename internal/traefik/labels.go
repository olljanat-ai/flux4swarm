// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

// Package traefik converts between Traefik Docker Swarm provider labels and
// Kubernetes Gateway API HTTPRoutes (plus Traefik Middleware CRDs for
// middlewares that have no Gateway API filter).
package traefik

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

// Gateway defaults used by import and as fallback on reconcile.
const (
	GatewayName      = "traefik"
	GatewayNamespace = "traefik"
	GatewayClass     = "traefik"
	ControllerName   = "traefik.io/gateway-controller"
)

// Backend resolves a Kubernetes Service port to the Swarm service that
// runs its pods and the container port.
type Backend func(namespace, service string, port int32) (swarmService string, targetPort uint32, err error)

// RouteLabels turns the HTTPRoutes of set into Traefik labels, keyed by
// Swarm service name.
func RouteLabels(set *api.Set, secrets []*corev1.Secret, backend Backend) (map[string]map[string]string, []string, error) {
	out := map[string]map[string]string{}
	var warnings []string
	listeners := map[string]gatewayv1.Listener{}
	gwListeners := map[string][]gatewayv1.Listener{}
	for _, g := range set.Gateways {
		for _, l := range g.Spec.Listeners {
			listeners[g.Namespace+"/"+g.Name+"/"+string(l.Name)] = l
			gwListeners[g.Namespace+"/"+g.Name] = append(gwListeners[g.Namespace+"/"+g.Name], l)
		}
	}
	mws := map[string]*api.TraefikMiddleware{}
	for _, m := range set.Middlewares {
		mws[m.Metadata.Namespace+"/"+m.Metadata.Name] = m
	}
	secretMap := map[string]*corev1.Secret{}
	for _, s := range secrets {
		secretMap[s.Namespace+"/"+s.Name] = s
	}

	routes := append([]*gatewayv1.HTTPRoute(nil), set.HTTPRoutes...)
	sort.Slice(routes, func(i, j int) bool {
		return routes[i].Namespace+"/"+routes[i].Name < routes[j].Namespace+"/"+routes[j].Name
	})
	for _, r := range routes {
		id := r.Namespace + "/" + r.Name
		var eps []string
		tls := false
		for _, p := range r.Spec.ParentRefs {
			if p.Kind != nil && *p.Kind != "Gateway" {
				continue
			}
			ns := r.Namespace
			if p.Namespace != nil {
				ns = string(*p.Namespace)
			}
			gw := ns + "/" + string(p.Name)
			if p.SectionName != nil {
				name := string(*p.SectionName)
				eps = append(eps, name)
				if l, ok := listeners[gw+"/"+name]; ok {
					tls = tls || l.Protocol == gatewayv1.HTTPSProtocolType
				} else if name == "websecure" {
					tls = true
				}
				continue
			}
			for _, l := range gwListeners[gw] {
				eps = append(eps, string(l.Name))
				tls = tls || l.Protocol == gatewayv1.HTTPSProtocolType
			}
		}
		eps = uniq(eps)
		var hosts []string
		for _, h := range r.Spec.Hostnames {
			hosts = append(hosts, "Host(`"+string(h)+"`)")
		}
		for i, rule := range r.Spec.Rules {
			name := r.Name
			if len(r.Spec.Rules) > 1 {
				name = fmt.Sprintf("%s-%d", r.Name, i)
			}
			if len(rule.BackendRefs) == 0 {
				warnings = append(warnings, fmt.Sprintf("HTTPRoute %s rule %d has no backendRefs; skipped", id, i))
				continue
			}
			if len(rule.BackendRefs) > 1 {
				warnings = append(warnings, fmt.Sprintf("HTTPRoute %s rule %d: only the first backendRef is used on Swarm", id, i))
			}
			br := rule.BackendRefs[0]
			if br.Kind != nil && *br.Kind != "Service" {
				return nil, warnings, fmt.Errorf("HTTPRoute %s: backendRef kind %s is not supported", id, *br.Kind)
			}
			bns := r.Namespace
			if br.Namespace != nil {
				bns = string(*br.Namespace)
			}
			port := int32(0)
			if br.Port != nil {
				port = int32(*br.Port)
			}
			svc, target, err := backend(bns, string(br.Name), port)
			if err != nil {
				return nil, warnings, fmt.Errorf("HTTPRoute %s: %w", id, err)
			}
			l := out[svc]
			if l == nil {
				l = map[string]string{}
				out[svc] = l
			}
			l["traefik.enable"] = "true"
			prefix := "traefik.http.routers." + name + "."
			l[prefix+"rule"] = buildRule(hosts, rule.Matches)
			if len(eps) > 0 {
				l[prefix+"entrypoints"] = strings.Join(eps, ",")
			}
			l[prefix+"service"] = name
			if tls {
				l[prefix+"tls"] = "true"
			}
			if cr := r.Annotations[api.AnnotationTraefikCertResolver]; cr != "" {
				l[prefix+"tls.certresolver"] = cr
			}
			if p := r.Annotations[api.AnnotationTraefikPriority]; p != "" {
				l[prefix+"priority"] = p
			}
			l["traefik.http.services."+name+".loadbalancer.server.port"] = strconv.FormatUint(uint64(target), 10)

			var mwNames []string
			for j, f := range rule.Filters {
				mname, labels, err := filterLabels(name, j, f, rule.Matches, r.Namespace, mws, secretMap)
				if err != nil {
					return nil, warnings, fmt.Errorf("HTTPRoute %s: %w", id, err)
				}
				if mname == "" {
					warnings = append(warnings, fmt.Sprintf("HTTPRoute %s: filter %s is not supported on Swarm; skipped", id, f.Type))
					continue
				}
				mwNames = append(mwNames, mname)
				for k, v := range labels {
					l[k] = v
				}
			}
			if len(mwNames) > 0 {
				l[prefix+"middlewares"] = strings.Join(mwNames, ",")
			}
		}
	}
	return out, warnings, nil
}

func buildRule(hosts []string, matches []gatewayv1.HTTPRouteMatch) string {
	var parts []string
	if len(hosts) == 1 {
		parts = append(parts, hosts[0])
	} else if len(hosts) > 1 {
		parts = append(parts, "("+strings.Join(hosts, " || ")+")")
	}
	var alts []string
	for _, m := range matches {
		var terms []string
		if p := m.Path; p != nil && p.Value != nil {
			t := gatewayv1.PathMatchPathPrefix
			if p.Type != nil {
				t = *p.Type
			}
			switch t {
			case gatewayv1.PathMatchExact:
				terms = append(terms, "Path(`"+*p.Value+"`)")
			case gatewayv1.PathMatchRegularExpression:
				terms = append(terms, "PathRegexp(`"+*p.Value+"`)")
			default:
				if *p.Value != "/" || (len(hosts) == 0 && len(matches) == 1 && m.Method == nil && len(m.Headers) == 0 && len(m.QueryParams) == 0) {
					terms = append(terms, "PathPrefix(`"+*p.Value+"`)")
				}
			}
		}
		if m.Method != nil {
			terms = append(terms, "Method(`"+string(*m.Method)+"`)")
		}
		for _, h := range m.Headers {
			if h.Type != nil && *h.Type == gatewayv1.HeaderMatchRegularExpression {
				terms = append(terms, "HeaderRegexp(`"+string(h.Name)+"`, `"+h.Value+"`)")
			} else {
				terms = append(terms, "Header(`"+string(h.Name)+"`, `"+h.Value+"`)")
			}
		}
		for _, q := range m.QueryParams {
			if q.Type != nil && *q.Type == gatewayv1.QueryParamMatchRegularExpression {
				terms = append(terms, "QueryRegexp(`"+string(q.Name)+"`, `"+q.Value+"`)")
			} else {
				terms = append(terms, "Query(`"+string(q.Name)+"`, `"+q.Value+"`)")
			}
		}
		if len(terms) > 0 {
			alts = append(alts, strings.Join(terms, " && "))
		}
	}
	switch {
	case len(alts) == 1:
		parts = append(parts, alts[0])
	case len(alts) > 1:
		for i, a := range alts {
			if strings.Contains(a, "&&") {
				alts[i] = "(" + a + ")"
			}
		}
		parts = append(parts, "("+strings.Join(alts, " || ")+")")
	}
	if len(parts) == 0 {
		return "PathPrefix(`/`)"
	}
	return strings.Join(parts, " && ")
}

func filterLabels(router string, idx int, f gatewayv1.HTTPRouteFilter, matches []gatewayv1.HTTPRouteMatch, ns string,
	mws map[string]*api.TraefikMiddleware, secrets map[string]*corev1.Secret) (string, map[string]string, error) {
	l := map[string]string{}
	switch f.Type {
	case gatewayv1.HTTPRouteFilterRequestRedirect:
		rr := f.RequestRedirect
		if rr == nil || rr.Scheme == nil {
			return "", nil, nil
		}
		name := fmt.Sprintf("%s-redirect", router)
		p := "traefik.http.middlewares." + name + ".redirectscheme."
		l[p+"scheme"] = *rr.Scheme
		if rr.StatusCode == nil || *rr.StatusCode == 301 || *rr.StatusCode == 308 {
			l[p+"permanent"] = "true"
		}
		if rr.Port != nil {
			l[p+"port"] = strconv.Itoa(int(*rr.Port))
		}
		return name, l, nil
	case gatewayv1.HTTPRouteFilterURLRewrite:
		rw := f.URLRewrite
		if rw == nil || rw.Path == nil {
			return "", nil, nil
		}
		switch rw.Path.Type {
		case gatewayv1.PrefixMatchHTTPPathModifier:
			repl := ""
			if rw.Path.ReplacePrefixMatch != nil {
				repl = *rw.Path.ReplacePrefixMatch
			}
			if repl != "" && repl != "/" {
				return "", nil, nil
			}
			var prefixes []string
			for _, m := range matches {
				if m.Path != nil && m.Path.Value != nil {
					prefixes = append(prefixes, *m.Path.Value)
				}
			}
			if len(prefixes) == 0 {
				return "", nil, nil
			}
			name := fmt.Sprintf("%s-stripprefix", router)
			l["traefik.http.middlewares."+name+".stripprefix.prefixes"] = strings.Join(prefixes, ",")
			return name, l, nil
		case gatewayv1.FullPathHTTPPathModifier:
			if rw.Path.ReplaceFullPath == nil {
				return "", nil, nil
			}
			name := fmt.Sprintf("%s-replacepath", router)
			l["traefik.http.middlewares."+name+".replacepath.path"] = *rw.Path.ReplaceFullPath
			return name, l, nil
		}
		return "", nil, nil
	case gatewayv1.HTTPRouteFilterRequestHeaderModifier, gatewayv1.HTTPRouteFilterResponseHeaderModifier:
		hm := f.RequestHeaderModifier
		field, suffix := "customrequestheaders", "reqheaders"
		if f.Type == gatewayv1.HTTPRouteFilterResponseHeaderModifier {
			hm, field, suffix = f.ResponseHeaderModifier, "customresponseheaders", "respheaders"
		}
		if hm == nil {
			return "", nil, nil
		}
		name := fmt.Sprintf("%s-%s", router, suffix)
		if idx > 0 {
			name = fmt.Sprintf("%s-%s-%d", router, suffix, idx)
		}
		p := "traefik.http.middlewares." + name + ".headers." + field + "."
		for _, h := range append(append([]gatewayv1.HTTPHeader(nil), hm.Set...), hm.Add...) {
			l[p+string(h.Name)] = h.Value
		}
		for _, h := range hm.Remove {
			l[p+h] = ""
		}
		return name, l, nil
	case gatewayv1.HTTPRouteFilterExtensionRef:
		ref := f.ExtensionRef
		if ref == nil || ref.Kind != "Middleware" || (ref.Group != "traefik.io" && ref.Group != "traefik.containo.us") {
			return "", nil, nil
		}
		m := mws[ns+"/"+string(ref.Name)]
		if m == nil {
			return "", nil, fmt.Errorf("Middleware %s/%s not found", ns, ref.Name)
		}
		labels, err := MiddlewareLabels(string(ref.Name), m.Spec, func(name, key string) (string, error) {
			s := secrets[ns+"/"+name]
			if s == nil {
				return "", fmt.Errorf("secret %s/%s not found", ns, name)
			}
			return string(s.Data[key]), nil
		})
		if err != nil {
			return "", nil, fmt.Errorf("Middleware %s: %w", ref.Name, err)
		}
		return string(ref.Name), labels, nil
	}
	return "", nil, nil
}

// MiddlewareLabels flattens a Middleware spec into Docker labels.
// secret reads keys of Secrets referenced by basicAuth/digestAuth.
func MiddlewareLabels(name string, spec map[string]any, secret func(name, key string) (string, error)) (map[string]string, error) {
	out := map[string]string{}
	for typ, v := range spec {
		cfg, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: expected an object", typ)
		}
		lt := strings.ToLower(typ)
		if (lt == "basicauth" || lt == "digestauth") && cfg["secret"] != nil {
			cfg = copyAny(cfg)
			sn, _ := cfg["secret"].(string)
			delete(cfg, "secret")
			users, err := secret(sn, "users")
			if err != nil {
				return nil, err
			}
			var lines []string
			for _, ln := range strings.Split(users, "\n") {
				if ln = strings.TrimSpace(ln); ln != "" {
					lines = append(lines, ln)
				}
			}
			cfg["users"] = lines
		}
		if err := flatten("traefik.http.middlewares."+name+"."+lt, cfg, out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func flatten(prefix string, v any, out map[string]string) error {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			if err := flatten(prefix+"."+strings.ToLower(k), val, out); err != nil {
				return err
			}
		}
	case []any:
		var parts []string
		for _, e := range t {
			switch e.(type) {
			case map[string]any, []any:
				return fmt.Errorf("%s: lists of objects cannot be expressed as labels", prefix)
			}
			parts = append(parts, fmt.Sprint(e))
		}
		out[prefix] = strings.Join(parts, ",")
	case []string:
		out[prefix] = strings.Join(t, ",")
	case nil:
		out[prefix] = ""
	case float64:
		out[prefix] = strconv.FormatFloat(t, 'f', -1, 64)
	default:
		out[prefix] = fmt.Sprint(t)
	}
	return nil
}

func copyAny(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}

func uniq(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
