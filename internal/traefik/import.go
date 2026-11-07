// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package traefik

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/olljanat-ai/flux4swarm/internal/api"
)

// Router is one Traefik HTTP router read from labels.
type Router struct {
	Name         string
	Rule         string
	EntryPoints  []string
	Middlewares  []string
	TLS          bool
	CertResolver string
	Service      string
	Priority     string
}

// Parsed holds the Traefik HTTP configuration of one Swarm service.
type Parsed struct {
	Routers     []*Router
	ServicePort map[string]string
	Middlewares map[string]map[string]string
}

// Parse reads traefik.http.* labels.
func Parse(labels map[string]string) *Parsed {
	p := &Parsed{ServicePort: map[string]string{}, Middlewares: map[string]map[string]string{}}
	routers := map[string]*Router{}
	for k, v := range labels {
		lk := strings.ToLower(k)
		switch {
		case strings.HasPrefix(lk, "traefik.http.routers."):
			rest := k[len("traefik.http.routers."):]
			name, field, ok := strings.Cut(rest, ".")
			if !ok {
				continue
			}
			r := routers[name]
			if r == nil {
				r = &Router{Name: name}
				routers[name] = r
			}
			switch strings.ToLower(field) {
			case "rule":
				r.Rule = v
			case "entrypoints":
				r.EntryPoints = splitList(v)
			case "middlewares":
				r.Middlewares = splitList(v)
			case "tls":
				r.TLS = v == "true"
			case "tls.certresolver":
				r.TLS = true
				r.CertResolver = v
			case "service":
				r.Service = v
			case "priority":
				r.Priority = v
			}
		case strings.HasPrefix(lk, "traefik.http.services.") && strings.HasSuffix(lk, ".loadbalancer.server.port"):
			name := k[len("traefik.http.services.") : len(k)-len(".loadbalancer.server.port")]
			p.ServicePort[name] = v
		case strings.HasPrefix(lk, "traefik.http.middlewares."):
			rest := k[len("traefik.http.middlewares."):]
			name, field, ok := strings.Cut(rest, ".")
			if !ok {
				continue
			}
			if p.Middlewares[name] == nil {
				p.Middlewares[name] = map[string]string{}
			}
			p.Middlewares[name][strings.ToLower(field)] = v
		}
	}
	for _, r := range routers {
		p.Routers = append(p.Routers, r)
	}
	sort.Slice(p.Routers, func(i, j int) bool { return p.Routers[i].Name < p.Routers[j].Name })
	return p
}

func splitList(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// ImportInput describes one Swarm service with Traefik labels.
type ImportInput struct {
	Namespace string
	// K8sService is the name of the Kubernetes Service that will point at
	// the workload.
	K8sService string
	Parsed     *Parsed
	// AllMiddlewares are the middlewares defined on every service, so that
	// routers can use middlewares declared on another service.
	AllMiddlewares map[string]map[string]string
}

// ImportResult are the Kubernetes objects for one service.
type ImportResult struct {
	Routes      []*gatewayv1.HTTPRoute
	Middlewares []*api.TraefikMiddleware
	// Ports the Kubernetes Service must expose (port == targetPort).
	Ports          []int32
	EntryPoints    []string
	TLSEntryPoints []string
	Warnings       []string
}

// Import converts the routers of one service to HTTPRoutes.
func Import(in ImportInput) *ImportResult {
	res := &ImportResult{}
	ports := map[int32]bool{}
	mwDone := map[string]bool{}
	for _, r := range in.Parsed.Routers {
		hosts, matches, err := ParseRule(r.Rule)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("router %s: %v; kept as labels", r.Name, err))
			continue
		}
		svcName := r.Service
		if svcName == "" && len(in.Parsed.ServicePort) == 1 {
			for k := range in.Parsed.ServicePort {
				svcName = k
			}
		}
		portStr, ok := in.Parsed.ServicePort[strings.TrimSuffix(strings.TrimSuffix(svcName, "@swarm"), "@docker")]
		if !ok {
			res.Warnings = append(res.Warnings, fmt.Sprintf("router %s: no loadbalancer.server.port for service %q; kept as labels", r.Name, svcName))
			continue
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("router %s: invalid port %q; kept as labels", r.Name, portStr))
			continue
		}
		ports[int32(port)] = true

		route := &gatewayv1.HTTPRoute{
			TypeMeta:   metav1.TypeMeta{APIVersion: "gateway.networking.k8s.io/v1", Kind: "HTTPRoute"},
			ObjectMeta: metav1.ObjectMeta{Name: r.Name, Namespace: in.Namespace},
		}
		gwNS := gatewayv1.Namespace(GatewayNamespace)
		if len(r.EntryPoints) == 0 {
			route.Spec.ParentRefs = []gatewayv1.ParentReference{{Name: GatewayName, Namespace: &gwNS}}
		}
		for _, ep := range r.EntryPoints {
			sec := gatewayv1.SectionName(ep)
			route.Spec.ParentRefs = append(route.Spec.ParentRefs, gatewayv1.ParentReference{Name: GatewayName, Namespace: &gwNS, SectionName: &sec})
			res.EntryPoints = append(res.EntryPoints, ep)
			if r.TLS {
				res.TLSEntryPoints = append(res.TLSEntryPoints, ep)
			}
		}
		for _, h := range hosts {
			route.Spec.Hostnames = append(route.Spec.Hostnames, gatewayv1.Hostname(h))
		}
		ann := map[string]string{}
		if r.CertResolver != "" {
			ann[api.AnnotationTraefikCertResolver] = r.CertResolver
		}
		if r.Priority != "" {
			ann[api.AnnotationTraefikPriority] = r.Priority
		}
		if len(ann) > 0 {
			route.Annotations = ann
		}
		p := gatewayv1.PortNumber(port)
		rule := gatewayv1.HTTPRouteRule{
			Matches: matches,
			BackendRefs: []gatewayv1.HTTPBackendRef{{BackendRef: gatewayv1.BackendRef{
				BackendObjectReference: gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(in.K8sService), Port: &p},
			}}},
		}
		for _, mref := range r.Middlewares {
			name := mref
			if i := strings.Index(mref, "@"); i >= 0 {
				prov := mref[i+1:]
				name = mref[:i]
				if prov != "swarm" && prov != "docker" {
					res.Warnings = append(res.Warnings, fmt.Sprintf("router %s: middleware %s comes from the %s provider; referenced as a Middleware CRD", r.Name, mref, prov))
				}
			}
			def := in.AllMiddlewares[name]
			f, mw := middlewareFilter(r.Name, name, def, matches)
			rule.Filters = append(rule.Filters, f)
			if mw != nil && !mwDone[name] {
				mw.Metadata.Namespace = in.Namespace
				res.Middlewares = append(res.Middlewares, mw)
				mwDone[name] = true
			}
		}
		route.Spec.Rules = []gatewayv1.HTTPRouteRule{rule}
		res.Routes = append(res.Routes, route)
	}
	for p := range ports {
		res.Ports = append(res.Ports, p)
	}
	sort.Slice(res.Ports, func(i, j int) bool { return res.Ports[i] < res.Ports[j] })
	return res
}

// middlewareFilter maps a middleware definition to a Gateway filter, or to
// an ExtensionRef plus a Middleware CRD.
func middlewareFilter(router, name string, def map[string]string, matches []gatewayv1.HTTPRouteMatch) (gatewayv1.HTTPRouteFilter, *api.TraefikMiddleware) {
	typ := ""
	for k := range def {
		t, _, _ := strings.Cut(k, ".")
		if typ != "" && t != typ {
			typ = "mixed"
			break
		}
		typ = t
	}
	switch typ {
	case "redirectscheme":
		if name == router+"-redirect" || onlyKeys(def, "redirectscheme.scheme", "redirectscheme.permanent") {
			if s := def["redirectscheme.scheme"]; s != "" && onlyKeys(def, "redirectscheme.scheme", "redirectscheme.permanent", "redirectscheme.port") && def["redirectscheme.port"] == "" {
				code := 302
				if def["redirectscheme.permanent"] == "true" {
					code = 301
				}
				return gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterRequestRedirect,
					RequestRedirect: &gatewayv1.HTTPRequestRedirectFilter{Scheme: &s, StatusCode: &code}}, nil
			}
		}
	case "stripprefix":
		if onlyKeys(def, "stripprefix.prefixes") {
			prefixes := splitList(def["stripprefix.prefixes"])
			if len(prefixes) == 1 && len(matches) == 1 && matches[0].Path != nil && matches[0].Path.Value != nil &&
				*matches[0].Path.Value == prefixes[0] {
				repl := "/"
				return gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterURLRewrite,
					URLRewrite: &gatewayv1.HTTPURLRewriteFilter{Path: &gatewayv1.HTTPPathModifier{
						Type: gatewayv1.PrefixMatchHTTPPathModifier, ReplacePrefixMatch: &repl}}}, nil
			}
		}
	case "headers":
		req, resp := &gatewayv1.HTTPHeaderFilter{}, &gatewayv1.HTTPHeaderFilter{}
		ok := true
		for k, v := range def {
			switch {
			case strings.HasPrefix(k, "headers.customrequestheaders."):
				h := k[len("headers.customrequestheaders."):]
				if v == "" {
					req.Remove = append(req.Remove, h)
				} else {
					req.Set = append(req.Set, gatewayv1.HTTPHeader{Name: gatewayv1.HTTPHeaderName(h), Value: v})
				}
			case strings.HasPrefix(k, "headers.customresponseheaders."):
				h := k[len("headers.customresponseheaders."):]
				if v == "" {
					resp.Remove = append(resp.Remove, h)
				} else {
					resp.Set = append(resp.Set, gatewayv1.HTTPHeader{Name: gatewayv1.HTTPHeaderName(h), Value: v})
				}
			default:
				ok = false
			}
		}
		// Only one direction fits into one filter.
		if ok && (len(req.Set)+len(req.Remove) == 0) != (len(resp.Set)+len(resp.Remove) == 0) {
			sortHeaders(req)
			sortHeaders(resp)
			if len(req.Set)+len(req.Remove) > 0 {
				return gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier, RequestHeaderModifier: req}, nil
			}
			return gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier, ResponseHeaderModifier: resp}, nil
		}
	}
	ext := gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterExtensionRef, ExtensionRef: &gatewayv1.LocalObjectReference{
		Group: "traefik.io", Kind: "Middleware", Name: gatewayv1.ObjectName(name)}}
	if def == nil {
		return ext, nil
	}
	return ext, &api.TraefikMiddleware{
		TypeMeta: api.TypeMeta{APIVersion: "traefik.io/v1alpha1", Kind: "Middleware"},
		Metadata: api.ObjectMeta{Name: name},
		Spec:     unflatten(def),
	}
}

func sortHeaders(f *gatewayv1.HTTPHeaderFilter) {
	sort.Slice(f.Set, func(i, j int) bool { return f.Set[i].Name < f.Set[j].Name })
	sort.Strings(f.Remove)
}

func onlyKeys(m map[string]string, keys ...string) bool {
	allowed := map[string]bool{}
	for _, k := range keys {
		allowed[k] = true
	}
	for k := range m {
		if !allowed[k] {
			return false
		}
	}
	return true
}

// unflatten turns "type.field.sub" keys into a Middleware spec, restoring
// Traefik's camelCase names where known. Comma lists become arrays for
// known list fields.
func unflatten(def map[string]string) map[string]any {
	out := map[string]any{}
	keys := make([]string, 0, len(def))
	for k := range def {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts := strings.Split(k, ".")
		cur := out
		for i, p := range parts {
			name := camel(p)
			// Header names inside custom*headers keep their case.
			if i > 0 && (strings.EqualFold(parts[i-1], "customrequestheaders") || strings.EqualFold(parts[i-1], "customresponseheaders")) {
				name = p
			}
			if i == len(parts)-1 {
				cur[name] = scalar(name, def[k])
				break
			}
			next, ok := cur[name].(map[string]any)
			if !ok {
				next = map[string]any{}
				cur[name] = next
			}
			cur = next
		}
	}
	return out
}

func scalar(field, v string) any {
	if listFields[strings.ToLower(field)] {
		return splitList(v)
	}
	if v == "true" || v == "false" {
		return v == "true"
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil && (v == "0" || !strings.HasPrefix(v, "0")) {
		return n
	}
	return v
}

var listFields = map[string]bool{
	"prefixes": true, "users": true, "sourcerange": true, "authresponseheaders": true,
	"authrequestheaders": true, "accesscontrolallowmethods": true, "accesscontrolallowheaders": true,
	"accesscontrolalloworiginlist": true, "accesscontrolexposeheaders": true, "allowedhosts": true,
	"hostsproxyheaders": true, "status": true, "middlewares": true, "excludedcontenttypes": true,
	"includedcontenttypes": true, "regex": false,
}

var camelNames = map[string]string{}

func init() {
	for _, n := range []string{
		"addPrefix", "basicAuth", "buffering", "chain", "circuitBreaker", "compress", "contentType",
		"digestAuth", "errors", "forwardAuth", "grpcWeb", "headers", "ipAllowList", "ipWhiteList",
		"inFlightReq", "passTLSClientCert", "rateLimit", "redirectRegex", "redirectScheme",
		"replacePath", "replacePathRegex", "retry", "stripPrefix", "stripPrefixRegex",
		"prefix", "users", "usersFile", "realm", "removeHeader", "headerField", "average", "burst",
		"period", "prefixes", "regex", "replacement", "scheme", "permanent", "port", "path",
		"attempts", "initialInterval", "address", "trustForwardHeader", "authResponseHeaders",
		"authResponseHeadersRegex", "authRequestHeaders", "sourceRange", "ipStrategy", "depth",
		"excludedIPs", "customRequestHeaders", "customResponseHeaders", "accessControlAllowCredentials",
		"accessControlAllowHeaders", "accessControlAllowMethods", "accessControlAllowOriginList",
		"accessControlAllowOriginListRegex", "accessControlExposeHeaders", "accessControlMaxAge",
		"addVaryHeader", "allowedHosts", "hostsProxyHeaders", "sslRedirect", "stsSeconds",
		"stsIncludeSubdomains", "stsPreload", "forceSTSHeader", "frameDeny", "customFrameOptionsValue",
		"contentTypeNosniff", "browserXssFilter", "contentSecurityPolicy", "referrerPolicy",
		"permissionsPolicy", "isDevelopment", "status", "service", "query", "amount", "expression",
		"checkPeriod", "fallbackDuration", "recoveryDuration", "excludedContentTypes",
		"includedContentTypes", "minResponseBodyBytes", "maxRequestBodyBytes", "memRequestBodyBytes",
		"maxResponseBodyBytes", "memResponseBodyBytes", "retryExpression", "autoDetect", "middlewares",
		"pem", "info", "sourceCriterion", "requestHeaderName", "requestHost", "maxDelay", "forceSlash",
	} {
		camelNames[strings.ToLower(n)] = n
	}
}

func camel(s string) string {
	if c, ok := camelNames[strings.ToLower(s)]; ok {
		return c
	}
	return s
}

// ParseRule parses the subset of Traefik rule syntax that maps to an
// HTTPRoute rule: Host terms (optionally OR-ed) AND-ed with at most one
// group of path, method, header and query matchers.
func ParseRule(rule string) ([]string, []gatewayv1.HTTPRouteMatch, error) {
	terms, err := splitTop(strings.TrimSpace(rule), "&&")
	if err != nil {
		return nil, nil, err
	}
	var hosts []string
	m := gatewayv1.HTTPRouteMatch{}
	var alts [][]matcher
	for _, t := range terms {
		t = strings.TrimSpace(t)
		inner := strings.TrimSpace(stripParens(t))
		ors, err := splitTop(inner, "||")
		if err != nil {
			return nil, nil, err
		}
		var group []matcher
		for _, o := range ors {
			mt, err := parseMatcher(strings.TrimSpace(stripParens(strings.TrimSpace(o))))
			if err != nil {
				return nil, nil, err
			}
			group = append(group, mt)
		}
		if allHost(group) {
			if len(hosts) > 0 {
				return nil, nil, fmt.Errorf("rule %q has several Host groups", rule)
			}
			for _, g := range group {
				hosts = append(hosts, g.args...)
			}
			continue
		}
		if len(group) > 1 {
			alts = append(alts, group)
			continue
		}
		if err := addTo(&m, group[0]); err != nil {
			return nil, nil, err
		}
	}
	if len(alts) > 1 {
		return nil, nil, fmt.Errorf("rule %q is too complex for an HTTPRoute", rule)
	}
	if len(alts) == 1 {
		var out []gatewayv1.HTTPRouteMatch
		for _, a := range alts[0] {
			c := *m.DeepCopy()
			if err := addTo(&c, a); err != nil {
				return nil, nil, err
			}
			out = append(out, c)
		}
		return hosts, out, nil
	}
	if m.Path == nil && m.Method == nil && len(m.Headers) == 0 && len(m.QueryParams) == 0 {
		return hosts, nil, nil
	}
	return hosts, []gatewayv1.HTTPRouteMatch{m}, nil
}

type matcher struct {
	fn   string
	args []string
}

func allHost(g []matcher) bool {
	for _, m := range g {
		if m.fn != "host" {
			return false
		}
	}
	return true
}

func addTo(m *gatewayv1.HTTPRouteMatch, mt matcher) error {
	switch mt.fn {
	case "host":
		return fmt.Errorf("Host must not be combined with other matchers in one group")
	case "pathprefix", "path", "pathregexp":
		if m.Path != nil || len(mt.args) != 1 {
			return fmt.Errorf("only one path matcher with one value is supported")
		}
		t := gatewayv1.PathMatchPathPrefix
		if mt.fn == "path" {
			t = gatewayv1.PathMatchExact
		} else if mt.fn == "pathregexp" {
			t = gatewayv1.PathMatchRegularExpression
		}
		v := mt.args[0]
		m.Path = &gatewayv1.HTTPPathMatch{Type: &t, Value: &v}
	case "method":
		if m.Method != nil || len(mt.args) != 1 {
			return fmt.Errorf("only one method is supported")
		}
		meth := gatewayv1.HTTPMethod(strings.ToUpper(mt.args[0]))
		m.Method = &meth
	case "header", "headerregexp":
		if len(mt.args) != 2 {
			return fmt.Errorf("Header needs a name and a value")
		}
		t := gatewayv1.HeaderMatchExact
		if mt.fn == "headerregexp" {
			t = gatewayv1.HeaderMatchRegularExpression
		}
		m.Headers = append(m.Headers, gatewayv1.HTTPHeaderMatch{Type: &t, Name: gatewayv1.HTTPHeaderName(mt.args[0]), Value: mt.args[1]})
	case "query", "queryregexp":
		if len(mt.args) != 2 {
			return fmt.Errorf("Query needs a name and a value")
		}
		t := gatewayv1.QueryParamMatchExact
		if mt.fn == "queryregexp" {
			t = gatewayv1.QueryParamMatchRegularExpression
		}
		m.QueryParams = append(m.QueryParams, gatewayv1.HTTPQueryParamMatch{Type: &t, Name: gatewayv1.HTTPHeaderName(mt.args[0]), Value: mt.args[1]})
	default:
		return fmt.Errorf("matcher %s has no HTTPRoute equivalent", mt.fn)
	}
	return nil
}

func parseMatcher(s string) (matcher, error) {
	open := strings.Index(s, "(")
	if open <= 0 || !strings.HasSuffix(s, ")") {
		return matcher{}, fmt.Errorf("cannot parse %q", s)
	}
	fn := strings.ToLower(strings.TrimSpace(s[:open]))
	if strings.HasPrefix(fn, "!") {
		return matcher{}, fmt.Errorf("negation is not supported")
	}
	var args []string
	body := s[open+1 : len(s)-1]
	for _, a := range strings.Split(body, ",") {
		a = strings.TrimSpace(a)
		if len(a) >= 2 && (a[0] == '`' || a[0] == '"') && a[len(a)-1] == a[0] {
			args = append(args, a[1:len(a)-1])
		} else if a != "" {
			return matcher{}, fmt.Errorf("cannot parse argument %q", a)
		}
	}
	return matcher{fn: fn, args: args}, nil
}

// splitTop splits s on op outside parentheses and quotes.
func splitTop(s, op string) ([]string, error) {
	var out []string
	depth, start := 0, 0
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '`' || c == '"':
			quote = c
		case c == '(':
			depth++
		case c == ')':
			depth--
			if depth < 0 {
				return nil, fmt.Errorf("unbalanced parentheses in %q", s)
			}
		case depth == 0 && strings.HasPrefix(s[i:], op):
			out = append(out, s[start:i])
			start = i + len(op)
			i += len(op) - 1
		}
	}
	if depth != 0 || quote != 0 {
		return nil, fmt.Errorf("unbalanced rule %q", s)
	}
	return append(out, s[start:]), nil
}

// stripParens removes one pair of parentheses enclosing the whole string.
func stripParens(s string) string {
	if !strings.HasPrefix(s, "(") || !strings.HasSuffix(s, ")") {
		return s
	}
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 && i != len(s)-1 {
				return s
			}
		}
	}
	return s[1 : len(s)-1]
}

// Gateway builds the GatewayClass and Gateway for a set of entrypoints.
func Gateway(entryPoints, tlsEntryPoints []string) (*gatewayv1.GatewayClass, *gatewayv1.Gateway) {
	gc := &gatewayv1.GatewayClass{
		TypeMeta:   metav1.TypeMeta{APIVersion: "gateway.networking.k8s.io/v1", Kind: "GatewayClass"},
		ObjectMeta: metav1.ObjectMeta{Name: GatewayClass},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: ControllerName},
	}
	gw := &gatewayv1.Gateway{
		TypeMeta:   metav1.TypeMeta{APIVersion: "gateway.networking.k8s.io/v1", Kind: "Gateway"},
		ObjectMeta: metav1.ObjectMeta{Name: GatewayName, Namespace: GatewayNamespace},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: GatewayClass},
	}
	tls := map[string]bool{}
	for _, e := range tlsEntryPoints {
		tls[e] = true
	}
	eps := uniq(append([]string(nil), entryPoints...))
	sort.Strings(eps)
	if len(eps) == 0 {
		eps = []string{"web"}
	}
	all := gatewayv1.NamespacesFromAll
	next := 8000
	for _, e := range eps {
		port, proto := 0, gatewayv1.HTTPProtocolType
		switch e {
		case "web", "http":
			port = 80
		case "websecure", "https":
			port = 443
		default:
			port = next
			next++
		}
		l := gatewayv1.Listener{
			Name:          gatewayv1.SectionName(e),
			Port:          gatewayv1.PortNumber(port),
			Protocol:      proto,
			AllowedRoutes: &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: &all}},
		}
		if tls[e] || e == "websecure" || e == "https" {
			l.Protocol = gatewayv1.HTTPSProtocolType
			mode := gatewayv1.TLSModeTerminate
			l.TLS = &gatewayv1.ListenerTLSConfig{Mode: &mode, CertificateRefs: []gatewayv1.SecretObjectReference{{Name: gatewayv1.ObjectName(e + "-tls")}}}
		}
		gw.Spec.Listeners = append(gw.Spec.Listeners, l)
	}
	return gc, gw
}

// SecretUsers returns the Secret for basicAuth users referenced from an
// imported Middleware, replacing the inline users list.
func SecretUsers(mw *api.TraefikMiddleware) *corev1.Secret {
	for _, typ := range []string{"basicAuth", "digestAuth"} {
		cfg, ok := mw.Spec[typ].(map[string]any)
		if !ok {
			continue
		}
		users, ok := cfg["users"].([]string)
		if !ok {
			continue
		}
		name := mw.Metadata.Name + "-users"
		delete(cfg, "users")
		cfg["secret"] = name
		return &corev1.Secret{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"},
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: mw.Metadata.Namespace},
			StringData: map[string]string{"users": strings.Join(users, "\n") + "\n"},
		}
	}
	return nil
}
