// Copyright 2015 Matthew Holt
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package certmagic

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/mholt/acmez/v3"
	"go.uber.org/zap"
	"golang.org/x/crypto/ocsp"
	"golang.org/x/net/idna"
)

// GetCertificate gets a certificate to satisfy clientHello. In getting
// the certificate, it abides the rules and settings defined in the Config
// that matches clientHello.ServerName. It tries to get certificates in
// this order:
//
// 1. Exact match in the in-memory cache
// 2. Wildcard match in the in-memory cache
// 3. Managers (if any)
// 4. Storage (if on-demand is enabled)
// 5. Issuers (if on-demand is enabled)
//
// This method is safe for use as a tls.Config.GetCertificate callback.
//
// GetCertificate will run in a new context, use GetCertificateWithContext to provide
// a context.
func (cfg *Config) GetCertificate(clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	return cfg.GetCertificateWithContext(clientHello.Context(), clientHello)
}

func (cfg *Config) GetCertificateWithContext(ctx context.Context, clientHello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	if err := cfg.emit(ctx, "tls_get_certificate", map[string]any{"client_hello": clientHelloWithoutConn(clientHello)}); err != nil {
		cfg.Logger.Error("TLS handshake aborted by event handler",
			zap.String("server_name", clientHello.ServerName),
			zap.String("remote", clientHello.Conn.RemoteAddr().String()),
			zap.Error(err))
		return nil, fmt.Errorf("handshake aborted by event handler: %w", err)
	}

	if ctx == nil {
		// tests can't set context on a tls.ClientHelloInfo because it's unexported :(
		ctx = context.Background()
	}
	ctx = context.WithValue(ctx, ClientHelloInfoCtxKey, clientHello)

	// special case: serve up the certificate for a TLS-ALPN ACME challenge
	// (https://www.rfc-editor.org/rfc/rfc8737.html)
	// "The ACME server MUST provide an ALPN extension with the single protocol
	// name "acme-tls/1" and an SNI extension containing only the domain name
	// being validated during the TLS handshake."
	if clientHello.ServerName != "" &&
		len(clientHello.SupportedProtos) == 1 &&
		clientHello.SupportedProtos[0] == acmez.ACMETLS1Protocol {
		challengeCert, distributed, err := cfg.getTLSALPNChallengeCert(clientHello)
		if err != nil {
			cfg.Logger.Error("tls-alpn challenge",
				zap.String("remote_addr", clientHello.Conn.RemoteAddr().String()),
				zap.String("server_name", clientHello.ServerName),
				zap.Error(err))
			return nil, err
		}
		cfg.Logger.Info("served key authentication certificate",
			zap.String("server_name", clientHello.ServerName),
			zap.String("challenge", "tls-alpn-01"),
			zap.String("remote", clientHello.Conn.RemoteAddr().String()),
			zap.Bool("distributed", distributed))
		return challengeCert, nil
	}

	// get the certificate and serve it up
	cert, err := cfg.getCertDuringHandshake(ctx, clientHello, true)

	return &cert.Certificate, err
}

// getCertificateFromCache gets a certificate that matches name from the in-memory
// cache, according to the lookup table associated with cfg. The lookup then
// points to a certificate in the Instance certificate cache.
//
// The name is expected to already be normalized (e.g. lowercased).
//
// If there is no exact match for name, it will be checked against names of
// the form '*.example.com' (wildcard certificates) according to RFC 6125.
// If a match is found, matched will be true. If no matches are found, matched
// will be false and a "default" certificate will be returned with defaulted
// set to true. If defaulted is false, then no certificates were available.
//
// The logic in this function is adapted from the Go standard library,
// which is by the Go Authors.
//
// This function is safe for concurrent use.
func (cfg *Config) getCertificateFromCache(hello *tls.ClientHelloInfo) (cert Certificate, matched, defaulted bool) {
	name := normalizedName(hello.ServerName)

	if name == "" {
		// if SNI is empty, prefer matching IP address
		if hello.Conn != nil {
			addr := localIPFromConn(hello.Conn)
			cert, matched = cfg.selectCert(hello, addr)
			if matched {
				return
			}
		}

		// use a "default" certificate by name, if specified
		if cfg.DefaultServerName != "" {
			normDefault := normalizedName(cfg.DefaultServerName)
			cert, defaulted = cfg.selectCert(hello, normDefault)
			if defaulted {
				return
			}
		}
	} else {
		// if SNI is specified, try an exact match first
		cert, matched = cfg.selectCert(hello, name)
		if matched {
			return
		}

		// try replacing labels in the name with
		// wildcards until we get a match
		labels := strings.Split(name, ".")
		for i := range labels {
			labels[i] = "*"
			candidate := strings.Join(labels, ".")
			cert, matched = cfg.selectCert(hello, candidate)
			if matched {
				return
			}
		}
	}

	// a fallback server name can be tried in the very niche
	// case where a client sends one SNI value but expects or
	// accepts a different one in return (this is sometimes
	// the case with CDNs like Cloudflare that send the
	// downstream ServerName in the handshake but accept
	// the backend origin's true hostname in a cert).
	if cfg.FallbackServerName != "" {
		normFallback := normalizedName(cfg.FallbackServerName)
		cert, defaulted = cfg.selectCert(hello, normFallback)
		if defaulted {
			return
		}
	}

	// otherwise, we're bingo on ammo; see issues
	// caddyserver/caddy#2035 and caddyserver/caddy#1303 (any
	// change to certificate matching behavior must
	// account for hosts defined where the hostname
	// is empty or a catch-all, like ":443" or
	// "0.0.0.0:443")

	return
}

// selectCert uses hello to select a certificate from the
// cache for name. If cfg.CertSelection is set, it will be
// used to make the decision. Otherwise, the first matching
// unexpired cert is returned. As a special case, if no
// certificates match name and cfg.CertSelection is set,
// then all certificates in the cache will be passed in
// for the cfg.CertSelection to make the final decision.
func (cfg *Config) selectCert(hello *tls.ClientHelloInfo, name string) (Certificate, bool) {
	logger := cfg.Logger.Named("handshake")
	choices := cfg.certCache.getAllMatchingCerts(name)

	if len(choices) == 0 {
		if cfg.CertSelection == nil {
			logger.Debug("no matching certificates and no custom selection logic", zap.String("identifier", name))
			return Certificate{}, false
		}
		logger.Debug("no matching certificate; will choose from all certificates", zap.String("identifier", name))
		choices = cfg.certCache.getAllCerts()
	}

	logger.Debug("choosing certificate",
		zap.String("identifier", name),
		zap.Int("num_choices", len(choices)))

	if cfg.CertSelection == nil {
		cert, err := DefaultCertificateSelector(hello, choices)
		logger.Debug("default certificate selection results",
			zap.Error(err),
			zap.String("identifier", name),
			zap.Strings("subjects", cert.Names),
			zap.Bool("managed", cert.managed),
			zap.String("issuer_key", cert.issuerKey),
			zap.String("hash", cert.hash))
		return cert, err == nil
	}

	cert, err := cfg.CertSelection.SelectCertificate(hello, choices)

	logger.Debug("custom certificate selection results",
		zap.Error(err),
		zap.String("identifier", name),
		zap.Strings("subjects", cert.Names),
		zap.Bool("managed", cert.managed),
		zap.String("issuer_key", cert.issuerKey),
		zap.String("hash", cert.hash))

	return cert, err == nil
}

// DefaultCertificateSelector is the default certificate selection logic
// given a choice of certificates. If there is at least one certificate in
// choices, it always returns a certificate without error. It chooses the
// first non-expired certificate that the client supports if possible,
// otherwise it returns an expired certificate that the client supports,
// otherwise it just returns the first certificate in the list of choices.
func DefaultCertificateSelector(hello *tls.ClientHelloInfo, choices []Certificate) (Certificate, error) {
	if len(choices) == 1 {
		// Fast path: There's only one choice, so we would always return that one
		// regardless of whether it is expired or not compatible.
		return choices[0], nil
	}
	if len(choices) == 0 {
		return Certificate{}, fmt.Errorf("no certificates available")
	}

	// Slow path: There are choices, so we need to check each of them.
	now := time.Now()
	best := choices[0]
	for _, choice := range choices {
		if err := hello.SupportsCertificate(&choice.Certificate); err != nil {
			continue
		}
		best = choice // at least the client supports it...
		if now.After(choice.Leaf.NotBefore) && now.Before(expiresAt(choice.Leaf)) {
			return choice, nil // ...and unexpired, great! "Certificate, I choose you!"
		}
	}
	return best, nil // all matching certs are expired or incompatible, oh well
}

// getCertDuringHandshake will get a certificate for hello. It first tries
// the in-memory cache. If no exact certificate for hello is in the cache, the
// config most closely corresponding to hello (like a wildcard) will be loaded.
// If none could be matched from the cache, it invokes the configured certificate
// managers to get a certificate and uses the first one that returns a certificate.
// If no certificate managers return a value, and if the config allows it
// (OnDemand!=nil) and if loadIfNecessary == true, it goes to storage to load the
// cert into the cache and serve it. If it's not on disk and if
// obtainIfNecessary == true, the certificate will be obtained from the CA, cached,
// and served. If obtainIfNecessary == true, then loadIfNecessary must also be == true.
// An error will be returned if and only if no certificate is available.
//
// This function is safe for concurrent use.
func (cfg *Config) getCertDuringHandshake(ctx context.Context, hello *tls.ClientHelloInfo, loadOrObtainIfNecessary bool) (Certificate, error) {
	logger := logWithRemote(cfg.Logger.Named("handshake"), hello)

	// First check our in-memory cache to see if we've already loaded it
	cert, matched, defaulted := cfg.getCertificateFromCache(hello)
	if matched {
		logger.Debug("matched certificate in cache",
			zap.Strings("subjects", cert.Names),
			zap.Bool("managed", cert.managed),
			zap.Time("expiration", expiresAt(cert.Leaf)),
			zap.String("hash", cert.hash))
		if cert.managed && cfg.OnDemand != nil && loadOrObtainIfNecessary {
			// On-demand certificates are maintained in the background, but
			// maintenance is triggered by handshakes instead of by a timer
			// as in maintain.go.
			return cfg.optionalMaintenance(ctx, cfg.Logger.Named("on_demand"), cert, hello)
		}
		return cert, nil
	}

	name, err := cfg.getNameFromClientHello(hello)
	if err != nil {
		return Certificate{}, err
	}

	// By this point, we need to load or obtain a certificate. If a swarm of requests comes in for the same
	// domain, avoid pounding manager or storage thousands of times simultaneously. We use a similar sync
	// strategy for obtaining certificate during handshake.
	certLoadWaitChansMu.Lock()
	wait, ok := certLoadWaitChans[name]
	if ok {
		// another goroutine is already loading the cert; just wait and we'll get it from the in-memory cache
		certLoadWaitChansMu.Unlock()

		timeout := time.NewTimer(2 * time.Minute)
		select {
		case <-timeout.C:
			return Certificate{}, fmt.Errorf("timed out waiting to load certificate for %s", name)
		case <-ctx.Done():
			timeout.Stop()
			return Certificate{}, ctx.Err()
		case <-wait:
			timeout.Stop()
		}

		return cfg.getCertDuringHandshake(ctx, hello, false)
	} else {
		// no other goroutine is currently trying to load this cert
		wait = make(chan struct{})
		certLoadWaitChans[name] = wait
		certLoadWaitChansMu.Unlock()

		// unblock others and clean up when we're done
		defer func() {
			certLoadWaitChansMu.Lock()
			close(wait)
			delete(certLoadWaitChans, name)
			certLoadWaitChansMu.Unlock()
		}()
	}

	// If an external Manager is configured, try to get it from them.
	// Only continue to use our own logic if it returns empty+nil.
	externalCert, err := cfg.getCertFromAnyCertManager(ctx, hello, logger)
	if err != nil {
		return Certificate{}, err
	}
	if !externalCert.Empty() {
		return externalCert, nil
	}

	// Make sure a certificate is allowed for the given name. If not, it doesn't make sense
	// to try loading one from storage (issue #185) or obtaining one from an issuer.
	if err := cfg.checkIfCertShouldBeObtained(ctx, name, false); err != nil {
		return Certificate{}, fmt.Errorf("certificate is not allowed for server name %s: %w", name, err)
	}

	// We might be able to load or obtain a needed certificate. Load from
	// storage if OnDemand is enabled, or if there is the possibility that
	// a statically-managed cert was evicted from a full cache.
	cfg.certCache.mu.RLock()
	cacheSize := len(cfg.certCache.cache)
	cfg.certCache.mu.RUnlock()

	// A cert might have still been evicted from the cache even if the cache
	// is no longer completely full; this happens if the newly-loaded cert is
	// itself evicted (perhaps due to being expired or unmanaged at this point).
	// Hence, we use an "almost full" metric to allow for the cache to not be
	// perfectly full while still being able to load needed certs from storage.
	// See https://caddy.community/t/error-tls-alert-internal-error-592-again/13272
	// and caddyserver/caddy#4320.
	cfg.certCache.optionsMu.RLock()
	cacheCapacity := float64(cfg.certCache.options.Capacity)
	cfg.certCache.optionsMu.RUnlock()
	cacheAlmostFull := cacheCapacity > 0 && float64(cacheSize) >= cacheCapacity*.9
	loadDynamically := cfg.OnDemand != nil || cacheAlmostFull

	if loadDynamically && loadOrObtainIfNecessary {
		// Check to see if we have one on disk
		loadedCert, err := cfg.loadCertFromStorage(ctx, logger, hello)
		if err == nil {
			return loadedCert, nil
		}
		logger.Debug("did not load cert from storage",
			zap.String("server_name", hello.ServerName),
			zap.Error(err))
		if cfg.OnDemand != nil {
			// By this point, we need to ask the CA for a certificate
			return cfg.obtainOnDemandCertificate(ctx, hello)
		}
		return loadedCert, nil
	}

	// Fall back to another certificate if there is one (either DefaultServerName or FallbackServerName)
	if defaulted {
		logger.Debug("fell back to default certificate",
			zap.Strings("subjects", cert.Names),
			zap.Bool("managed", cert.managed),
			zap.Time("expiration", expiresAt(cert.Leaf)),
			zap.String("hash", cert.hash))
		return cert, nil
	}

	logger.Debug("no certificate matching TLS ClientHello",
		zap.String("server_name", hello.ServerName),
		zap.String("remote", hello.Conn.RemoteAddr().String()),
		zap.String("identifier", name),
		zap.Uint16s("cipher_suites", hello.CipherSuites),
		zap.Float64("cert_cache_fill", float64(cacheSize)/cacheCapacity), // may be approximate! because we are not within the lock
		zap.Bool("load_or_obtain_if_necessary", loadOrObtainIfNecessary),
		zap.Bool("on_demand", cfg.OnDemand != nil))

	return Certificate{}, fmt.Errorf("no certificate available for '%s'", name)
}

// loadCertFromStorage loads the certificate for name from storage and maintains it
// (as this is only called with on-demand TLS enabled).
func (cfg *Config) loadCertFromStorage(ctx context.Context, logger *zap.Logger, hello *tls.ClientHelloInfo) (Certificate, error) {
	name, err := cfg.getNameFromClientHello(hello)
	if err != nil {
		return Certificate{}, err
	}
	loadedCert, err := cfg.CacheManagedCertificate(ctx, name)
	if errors.Is(err, fs.ErrNotExist) {
		// If no exact match, try a wildcard variant, which is something we can still use
		labels := strings.Split(name, ".")
		labels[0] = "*"
		loadedCert, err = cfg.CacheManagedCertificate(ctx, strings.Join(labels, "."))
	}
	if err != nil {
		return Certificate{}, fmt.Errorf("no matching certificate to load for %s: %w", name, err)
	}
	logger.Debug("loaded certificate from storage",
		zap.Strings("subjects", loadedCert.Names),
		zap.Bool("managed", loadedCert.managed),
		zap.Time("expiration", expiresAt(loadedCert.Leaf)),
		zap.String("hash", loadedCert.hash))
	loadedCert, err = cfg.handshakeMaintenance(ctx, hello, loadedCert)
	if err != nil {
		logger.Error("maintaining newly-loaded certificate",
			zap.String("server_name", name),
			zap.Error(err))
	}
	return loadedCert, nil
}

// optionalMaintenance will perform maintenance on the certificate (if necessary) and
// will return the resulting certificate. This should only be done if the certificate
// is managed, OnDemand is enabled, and the scope is allowed to obtain certificates.
func (cfg *Config) optionalMaintenance(ctx context.Context, log *zap.Logger, cert Certificate, hello *tls.ClientHelloInfo) (Certificate, error) {
	newCert, err := cfg.handshakeMaintenance(ctx, hello, cert)
	if err == nil {
		return newCert, nil
	}

	log.Error("renewing certificate on-demand failed",
		zap.Strings("subjects", cert.Names),
		zap.Time("not_after", expiresAt(cert.Leaf)),
		zap.Error(err))

	if cert.Expired() {
		return cert, err
	}

	// still has time remaining, so serve it anyway
	return cert, nil
}

// checkIfCertShouldBeObtained checks to see if an on-demand TLS certificate
// should be obtained for a given domain based upon the config settings. If
// a non-nil error is returned, do not issue a new certificate for name.
func (cfg *Config) checkIfCertShouldBeObtained(ctx context.Context, name string, requireOnDemand bool) error {
	if requireOnDemand && cfg.OnDemand == nil {
		return fmt.Errorf("not configured for on-demand certificate issuance")
	}
	if !SubjectQualifiesForCert(name) {
		return fmt.Errorf("subject name does not qualify for certificate: %s", name)
	}
	if cfg.OnDemand != nil {
		if cfg.OnDemand.DecisionFunc != nil {
			if err := cfg.OnDemand.DecisionFunc(ctx, name); err != nil {
				return fmt.Errorf("decision func: %w", err)
			}
			return nil
		}
		if len(cfg.OnDemand.hostAllowlist) > 0 {
			if _, ok := cfg.OnDemand.hostAllowlist[name]; !ok {
				return fmt.Errorf("certificate for '%s' is not managed", name)
			}
		}
	}
	return nil
}

// obtainOnDemandCertificate obtains a certificate for hello.
// If another goroutine has already started obtaining a cert for
// hello, it will wait and use what the other goroutine obtained.
//
// This function is safe for use by multiple concurrent goroutines.
func (cfg *Config) obtainOnDemandCertificate(ctx context.Context, hello *tls.ClientHelloInfo) (Certificate, error) {
	log := logWithRemote(cfg.Logger.Named("on_demand"), hello)

	name, err := cfg.getNameFromClientHello(hello)
	if err != nil {
		return Certificate{}, err
	}

	// We must protect this process from happening concurrently, so synchronize.
	obtainCertWaitChansMu.Lock()
	wait, ok := obtainCertWaitChans[name]
	if ok {
		// lucky us -- another goroutine is already obtaining the certificate.
		// wait for it to finish obtaining the cert and then we'll use it.
		obtainCertWaitChansMu.Unlock()

		log.Debug("new certificate is needed, but is already being obtained; waiting for that issuance to complete",
			zap.String("subject", name))

		// TODO: see if we can get a proper context in here, for true cancellation
		timeout := time.NewTimer(2 * time.Minute)
		select {
		case <-timeout.C:
			return Certificate{}, fmt.Errorf("timed out waiting to obtain certificate for %s", name)
		case <-wait:
			timeout.Stop()
		}

		// it should now be loaded in the cache, ready to go; if not,
		// the goroutine in charge of that probably had an error
		return cfg.getCertDuringHandshake(ctx, hello, false)
	}

	// looks like it's up to us to do all the work and obtain the cert.
	// make a chan others can wait on if needed
	wait = make(chan struct{})
	obtainCertWaitChans[name] = wait
	obtainCertWaitChansMu.Unlock()

	unblockWaiters := func() {
		obtainCertWaitChansMu.Lock()
		close(wait)
		delete(obtainCertWaitChans, name)
		obtainCertWaitChansMu.Unlock()
	}

	log.Info("obtaining new certificate", zap.String("server_name", name))

	// set a timeout so we don't inadvertently hold a client handshake open too long
	// (timeout duration is based on https://caddy.community/t/zerossl-dns-challenge-failing-often-route53-plugin/13822/24?u=matt)
	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(ctx, 180*time.Second)
	defer cancel()

	// obtain the certificate (this puts it in storage) and if successful,
	// load it from storage so we and any other waiting goroutine can use it
	var cert Certificate
	err = cfg.ObtainCertAsync(ctx, name)
	if err == nil {
		// load from storage while others wait to make the op as atomic as possible
		cert, err = cfg.loadCertFromStorage(ctx, log, hello)
		if err != nil {
			log.Error("loading newly-obtained certificate from storage", zap.String("server_name", name), zap.Error(err))
		}
	}

	// immediately unblock anyone waiting for it
	unblockWaiters()

	return cert, err
}

// handshakeMaintenance performs a check on cert for expiration and OCSP validity.
// If necessary, it will renew the certificate and/or refresh the OCSP staple.
// OCSP stapling errors are not returned, only logged.
//
// This function is safe for use by multiple concurrent goroutines.
func (cfg *Config) handshakeMaintenance(ctx context.Context, hello *tls.ClientHelloInfo, cert Certificate) (Certificate, error) {
	logger := cfg.Logger.Named("on_demand").With(
		zap.Strings("identifiers", cert.Names),
		zap.String("server_name", hello.ServerName))

	renewIfNecessary := func(ctx context.Context, hello *tls.ClientHelloInfo, cert Certificate) (Certificate, error) {
		if cert.Leaf == nil {
			return cert, fmt.Errorf("leaf certificate is unexpectedly nil: either the Certificate got replaced by an empty value, or it was not properly initialized")
		}
		if cfg.certNeedsRenewal(cert.Leaf, cert.ari, true) {
			// Check if the certificate still exists on disk. If not, we need to obtain a new one.
			// This can happen if the certificate was cleaned up by the storage cleaner, but still
			// remains in the in-memory cache.
			if !cfg.storageHasCertResourcesAnyIssuer(ctx, cert.Names[0]) {
				logger.Debug("certificate not found on disk; obtaining new certificate")
				return cfg.obtainOnDemandCertificate(ctx, hello)
			}
			// Otherwise, renew the certificate.
			return cfg.renewDynamicCertificate(ctx, hello, cert)
		}
		return cert, nil
	}

	// Check OCSP staple validity
	if cert.ocsp != nil && !freshOCSP(cert.ocsp) {
		logger.Debug("OCSP response needs refreshing",
			zap.Int("ocsp_status", cert.ocsp.Status),
			zap.Time("this_update", cert.ocsp.ThisUpdate),
			zap.Time("next_update", cert.ocsp.NextUpdate))

		err := stapleOCSP(ctx, cfg.OCSP, cfg.Storage, &cert, nil)
		if err != nil {
			// An error with OCSP stapling is not the end of the world, and in fact, is
			// quite common considering not all certs have issuer URLs that support it.
			logger.Warn("stapling OCSP", zap.Error(err))
		} else {
			logger.Debug("successfully stapled new OCSP response",
				zap.Int("ocsp_status", cert.ocsp.Status),
				zap.Time("this_update", cert.ocsp.ThisUpdate),
				zap.Time("next_update", cert.ocsp.NextUpdate))
		}

		// our copy of cert has the new OCSP staple, so replace it in the cache
		cfg.certCache.mu.Lock()
		cfg.certCache.cache[cert.hash] = cert
		cfg.certCache.mu.Unlock()
	}

	// Check ARI status, but it's only relevant if the certificate is not expired (otherwise, we already know it needs renewal!)
	if !cfg.DisableARI && cert.ari.NeedsRefresh() && time.Now().Before(cert.Leaf.NotAfter) {
		// update ARI in a goroutine to avoid blocking an active handshake, since the results of
		// this do not strictly affect the handshake; even though the cert may be updated with
		// the new ARI, it is also updated in the cache and in storage, so future handshakes
		// will utilize it
		go func(hello *tls.ClientHelloInfo, cert Certificate, logger *zap.Logger) {
			// TODO: a different context that isn't tied to the handshake is probably better
			// than a generic background context; maybe a longer-lived server config context,
			// or something that the importing package sets on the Config struct; for example,
			// a Caddy config context could be good, so that ARI updates will continue after
			// the handshake goes away, but will be stopped if the underlying server is stopped
			// (for now, use an unusual timeout to help recognize it in log patterns, if needed)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
			defer cancel()

			var err error
			// we ignore the second return value here because we check renewal status below regardless
			cert, _, err = cfg.updateARI(ctx, cert, logger)
			if err != nil {
				logger.Error("updating ARI", zap.Error(err))
			}
			_, err = renewIfNecessary(ctx, hello, cert)
			if err != nil {
				logger.Error("renewing certificate based on updated ARI", zap.Error(err))
			}
		}(hello, cert, logger)
	}

	// We attempt to replace any certificates that were revoked.
	// Crucially, this happens OUTSIDE a lock on the certCache.
	if certShouldBeForceRenewed(cert) {
		logger.Warn("on-demand certificate's OCSP status is REVOKED; will try to forcefully renew",
			zap.Int("ocsp_status", cert.ocsp.Status),
			zap.Time("revoked_at", cert.ocsp.RevokedAt),
			zap.Time("this_update", cert.ocsp.ThisUpdate),
			zap.Time("next_update", cert.ocsp.NextUpdate))
		return cfg.renewDynamicCertificate(ctx, hello, cert)
	}

	// Since renewal conditions may have changed, do a renewal if necessary
	return renewIfNecessary(ctx, hello, cert)
}

// renewDynamicCertificate renews the certificate for name using cfg. It returns the
// certificate to use and an error, if any. name should already be lower-cased before
// calling this function. name is the name obtained directly from the handshake's
// ClientHello. If the certificate hasn't yet expired, currentCert will be returned
// and the renewal will happen in the background; otherwise this blocks until the
// certificate has been renewed, and returns the renewed certificate.
//
// If the certificate's OCSP status (currentCert.ocsp) is Revoked, it will be forcefully
// renewed even if it is not expiring.
//
// This function is safe for use by multiple concurrent goroutines.
func (cfg *Config) renewDynamicCertificate(ctx context.Context, hello *tls.ClientHelloInfo, currentCert Certificate) (Certificate, error) {
	logger := logWithRemote(cfg.Logger.Named("on_demand"), hello)

	name, err := cfg.getNameFromClientHello(hello)
	if err != nil {
		return Certificate{}, err
	}
	timeLeft := time.Until(expiresAt(currentCert.Leaf))
	revoked := currentCert.ocsp != nil && currentCert.ocsp.Status == ocsp.Revoked

	// see if another goroutine is already working on this certificate
	obtainCertWaitChansMu.Lock()
	wait, ok := obtainCertWaitChans[name]
	if ok {
		// lucky us -- another goroutine is already renewing the certificate
		obtainCertWaitChansMu.Unlock()

		// the current certificate hasn't expired, and another goroutine is already
		// renewing it, so we might as well serve what we have without blocking, UNLESS
		// we're forcing renewal, in which case the current certificate is not usable
		if timeLeft > 0 && !revoked {
			logger.Debug("certificate expires soon but is already being renewed; serving current certificate",
				zap.Strings("subjects", currentCert.Names),
				zap.Duration("remaining", timeLeft))
			return currentCert, nil
		}

		// otherwise, we'll have to wait for the renewal to finish so we don't serve
		// a revoked or expired certificate

		logger.Debug("certificate has expired, but is already being renewed; waiting for renewal to complete",
			zap.Strings("subjects", currentCert.Names),
			zap.Time("expired", expiresAt(currentCert.Leaf)),
			zap.Bool("revoked", revoked))

		// TODO: see if we can get a proper context in here, for true cancellation
		timeout := time.NewTimer(2 * time.Minute)
		select {
		case <-timeout.C:
			return Certificate{}, fmt.Errorf("timed out waiting for certificate renewal of %s", name)
		case <-wait:
			timeout.Stop()
		}

		// it should now be loaded in the cache, ready to go; if not,
		// the goroutine in charge of that probably had an error
		return cfg.getCertDuringHandshake(ctx, hello, false)
	}

	// looks like it's up to us to do all the work and renew the cert
	wait = make(chan struct{})
	obtainCertWaitChans[name] = wait
	obtainCertWaitChansMu.Unlock()

	unblockWaiters := func() {
		obtainCertWaitChansMu.Lock()
		close(wait)
		delete(obtainCertWaitChans, name)
		obtainCertWaitChansMu.Unlock()
	}

	logger = logger.With(
		zap.String("server_name", name),
		zap.Strings("subjects", currentCert.Names),
		zap.Time("expiration", expiresAt(currentCert.Leaf)),
		zap.Duration("remaining", timeLeft),
		zap.Bool("revoked", revoked),
	)

	// Renew and reload the certificate
	renewAndReload := func(ctx context.Context, cancel context.CancelFunc) (Certificate, error) {
		defer cancel()

		// Make sure a certificate for this name should be renewed on-demand
		err := cfg.checkIfCertShouldBeObtained(ctx, name, true)
		if err != nil {
			// if not, remove from cache (it will be deleted from storage later)
			cfg.certCache.mu.Lock()
			cfg.certCache.removeCertificate(currentCert)
			cfg.certCache.mu.Unlock()
			unblockWaiters()

			if logger != nil {
				logger.Error("certificate should not be obtained", zap.Error(err))
			}

			return Certificate{}, err
		}

		logger.Info("attempting certificate renewal")

		// otherwise, renew with issuer, etc.
		var newCert Certificate
		if revoked {
			newCert, err = cfg.forceRenew(ctx, logger, currentCert)
		} else {
			err = cfg.RenewCertAsync(ctx, name, false)
			if err == nil {
				// load from storage while in lock to make the replacement as atomic as possible
				newCert, err = cfg.reloadManagedCertificate(ctx, currentCert)
			}
		}

		// immediately unblock anyone waiting for it; doing this in
		// a defer would risk deadlock because of the recursive call
		// to getCertDuringHandshake below when we return!
		unblockWaiters()

		if err != nil {
			logger.Error("renewing and reloading certificate", zap.String("server_name", name), zap.Error(err))
		}

		return newCert, err
	}

	// if the certificate hasn't expired, we can serve what we have and renew in the background
	if timeLeft > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		go renewAndReload(ctx, cancel)
		return currentCert, nil
	}

	// otherwise, we have to block while we renew an expired certificate
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	return renewAndReload(ctx, cancel)
}

// getCertFromAnyCertManager gets a certificate from cfg's Managers. If there are no Managers defined, this is
// a no-op that returns empty values. Otherwise, it gets a certificate for hello from the first Manager that
// returns a certificate and no error.
func (cfg *Config) getCertFromAnyCertManager(ctx context.Context, hello *tls.ClientHelloInfo, logger *zap.Logger) (Certificate, error) {
	// fast path if nothing to do
	if cfg.OnDemand == nil || len(cfg.OnDemand.Managers) == 0 {
		return Certificate{}, nil
	}

	// try all the GetCertificate methods on external managers; use first one that returns a certificate
	var upstreamCert *tls.Certificate
	var err error
	for i, certManager := range cfg.OnDemand.Managers {
		upstreamCert, err = certManager.GetCertificate(ctx, hello)
		if err != nil {
			logger.Error("external certificate manager",
				zap.String("sni", hello.ServerName),
				zap.String("cert_manager", fmt.Sprintf("%T", certManager)),
				zap.Int("cert_manager_idx", i),
				zap.Error(err))
			continue
		}
		if upstreamCert != nil {
			break
		}
	}
	if err != nil {
		return Certificate{}, fmt.Errorf("external certificate manager indicated that it is unable to yield certificate: %v", err)
	}
	if upstreamCert == nil {
		logger.Debug("all external certificate managers yielded no certificates and no errors", zap.String("sni", hello.ServerName))
		return Certificate{}, nil
	}

	var cert Certificate
	if err = fillCertFromLeaf(&cert, *upstreamCert); err != nil {
		return Certificate{}, fmt.Errorf("external certificate manager: %s: filling cert from leaf: %v", hello.ServerName, err)
	}

	logger.Debug("using externally-managed certificate",
		zap.String("sni", hello.ServerName),
		zap.Strings("names", cert.Names),
		zap.Time("expiration", expiresAt(cert.Leaf)))

	return cert, nil
}

// getTLSALPNChallengeCert is to be called when the clientHello pertains to
// a TLS-ALPN challenge and a certificate is required to solve it. This method gets
// the relevant challenge info and then returns the associated certificate (if any)
// or generates it anew if it's not available (as is the case when distributed
// solving). True is returned if the challenge is being solved distributed (there
// is no semantic difference with distributed solving; it is mainly for logging).
func (cfg *Config) getTLSALPNChallengeCert(clientHello *tls.ClientHelloInfo) (*tls.Certificate, bool, error) {
	chalData, distributed, err := cfg.getChallengeInfo(clientHello.Context(), clientHello.ServerName)
	if err != nil {
		return nil, distributed, err
	}

	// fast path: we already created the certificate (this avoids having to re-create
	// it at every handshake that tries to verify, e.g. multi-perspective validation)
	if chalData.data != nil {
		return chalData.data.(*tls.Certificate), distributed, nil
	}

	// otherwise, we can re-create the solution certificate, but it takes a few cycles
	cert, err := acmez.TLSALPN01ChallengeCert(chalData.Challenge)
	if err != nil {
		return nil, distributed, fmt.Errorf("making TLS-ALPN challenge certificate: %v", err)
	}
	if cert == nil {
		return nil, distributed, fmt.Errorf("got nil TLS-ALPN challenge certificate but no error")
	}

	return cert, distributed, nil
}

// getNameFromClientHello returns a normalized form of hello.ServerName.
// If hello.ServerName is empty (i.e. client did not use SNI), then the
// associated connection's local address is used to extract an IP address.
func (cfg *Config) getNameFromClientHello(hello *tls.ClientHelloInfo) (string, error) {
	// IDNs must be converted to punycode for use in TLS certificates (and SNI), but not
	// all clients do that, so convert IDNs to ASCII according to RFC 5280 section 7
	// using profile recommended by RFC 5891 section 5; this solves the "σςΣ" problem
	// (see https://unicode.org/faq/idn.html#22) where not all normalizations are 1:1.
	// The Lookup profile, for instance, rejects wildcard characters (*), but they
	// should never be used in the ClientHello SNI anyway.
	name, err := idna.Lookup.ToASCII(strings.TrimSpace(hello.ServerName))
	if err != nil {
		return "", err
	}
	if name != "" {
		return name, nil
	}
	if cfg.DefaultServerName != "" {
		return normalizedName(cfg.DefaultServerName), nil
	}
	return localIPFromConn(hello.Conn), nil
}

// logWithRemote adds the remote host and port to the logger.
func logWithRemote(l *zap.Logger, hello *tls.ClientHelloInfo) *zap.Logger {
	if hello.Conn == nil || l == nil {
		return l
	}
	addr := hello.Conn.RemoteAddr().String()
	ip, port, err := net.SplitHostPort(addr)
	if err != nil {
		ip = addr
		port = ""
	}
	return l.With(zap.String("remote_ip", ip), zap.String("remote_port", port))
}

// localIPFromConn returns the host portion of c's local address
// and strips the scope ID if one exists (see RFC 4007).
func localIPFromConn(c net.Conn) string {
	if c == nil {
		return ""
	}
	localAddr := c.LocalAddr().String()
	ip, _, err := net.SplitHostPort(localAddr)
	if err != nil {
		// OK; assume there was no port
		ip = localAddr
	}
	// IPv6 addresses can have scope IDs, e.g. "fe80::4c3:3cff:fe4f:7e0b%eth0",
	// but for our purposes, these are useless (unless a valid use case proves
	// otherwise; see issue #3911)
	if scopeIDStart := strings.Index(ip, "%"); scopeIDStart > -1 {
		ip = ip[:scopeIDStart]
	}
	return ip
}

// normalizedName returns a cleaned form of serverName that is
// used for consistency when referring to a SNI value.
func normalizedName(serverName string) string {
	return strings.ToLower(strings.TrimSpace(serverName))
}

// obtainCertWaitChans is used to coordinate obtaining certs for each hostname.
var (
	obtainCertWaitChans   = make(map[string]chan struct{})
	obtainCertWaitChansMu sync.Mutex
)

// TODO: this lockset should probably be per-cache
var (
	certLoadWaitChans   = make(map[string]chan struct{})
	certLoadWaitChansMu sync.Mutex
)

type serializableClientHello struct {
	CipherSuites      []uint16
	ServerName        string
	SupportedCurves   []tls.CurveID
	SupportedPoints   []uint8
	SignatureSchemes  []tls.SignatureScheme
	SupportedProtos   []string
	SupportedVersions []uint16

	RemoteAddr, LocalAddr net.Addr // values copied from the Conn as they are still useful/needed
	conn                  net.Conn // unexported so it's not serialized
}

// clientHelloWithoutConn returns the data from the ClientHelloInfo without the
// pesky exported Conn field, which often causes an error when serializing because
// the underlying type may be unserializable.
func clientHelloWithoutConn(hello *tls.ClientHelloInfo) serializableClientHello {
	if hello == nil {
		return serializableClientHello{}
	}
	var remote, local net.Addr
	if hello.Conn != nil {
		remote = hello.Conn.RemoteAddr()
		local = hello.Conn.LocalAddr()
	}
	return serializableClientHello{
		CipherSuites:      hello.CipherSuites,
		ServerName:        hello.ServerName,
		SupportedCurves:   hello.SupportedCurves,
		SupportedPoints:   hello.SupportedPoints,
		SignatureSchemes:  hello.SignatureSchemes,
		SupportedProtos:   hello.SupportedProtos,
		SupportedVersions: hello.SupportedVersions,
		RemoteAddr:        remote,
		LocalAddr:         local,
		conn:              hello.Conn,
	}
}

type helloInfoCtxKey string

// ClientHelloInfoCtxKey is the key by which the ClientHelloInfo can be extracted from
// a context.Context within a DecisionFunc. However, be advised that it is best practice
// that the decision whether to obtain a certificate is be based solely on the name,
// not other properties of the specific connection/client requesting the connection.
// For example, it is not advisable to use a client's IP address to decide whether to
// allow a certificate. Instead, the ClientHello can be useful for logging, etc.
const ClientHelloInfoCtxKey helloInfoCtxKey = "certmagic:ClientHelloInfo"
