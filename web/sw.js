/* Offline-capable app shell for the Homelab Monitor PWA.
 *
 * Strategy:
 *   - The static shell (HTML/CSS/JS + manifest + icon) is cached first, so a
 *     refresh while offline still renders the dashboard/login.
 *   - Dynamic requests (e.g. GET /api/fleet) go to the network and, on
 *     failure, fall back to the last cached response.
 *   - install() pre-populates the shell cache; activate() drops older cache
 *     versions and claims the client so the new shell serves immediately.
 */

const CACHE_VERSION = "v1";
const SHELL_CACHE = CACHE_VERSION + ":shell";
const API_CACHE = CACHE_VERSION + ":api";

// App shell assets to cache up front. Kept in sync with index.html.
const SHELL = [
  "index.html",
  "login.html",
  "app.js",
  "login.js",
  "style.css",
  "manifest.json",
  "icon.svg",
];

self.addEventListener("install", (event) => {
  event.waitUntil(
    caches.open(SHELL_CACHE).then((cache) => cache.addAll(SHELL))
  );
});

self.addEventListener("activate", (event) => {
  event.waitUntil(
    caches
      .keys()
      .then((keys) =>
        Promise.all(
          keys.filter((key) => key.startsWith(CACHE_VERSION + ":")).map(
            (key) => (key === SHELL_CACHE ? Promise.resolve(key) : caches.delete(key))
          )
        )
      )
  );
  self.clients.claim();
});

self.addEventListener("fetch", (event) => {
  if (event.request.method !== "GET") return;

  const { pathname } = new URL(event.request.url, self.location.origin);
  // The root path ("/") is how start_url actually gets requested, and it
  // serves the same content as index.html - route it through the same
  // cache entry rather than treating it as a miss.
  const shellPath = pathname === "/" ? "index.html" : pathname.replace(/^\//, "");
  const isShell = SHELL.includes(shellPath);

  if (isShell) {
    // cache.addAll(SHELL) stored each asset under its own resolved URL, so
    // look up by that URL rather than the (possibly "/") request URL.
    const shellURL = new URL(shellPath, self.location.origin).toString();
    event.respondWith(
      caches.match(shellURL).then((cached) =>
        cached ||
          fetch(event.request).then((resp) => {
            if (!resp || resp.status >= 400) return resp;
            return caches
              .open(SHELL_CACHE)
              .then((cache) => cache.put(shellURL, resp.clone()))
              .then(() => resp);
          })
      )
    );
    return;
  }

  // Network-first for dynamic requests, with a cached fallback.
  event.respondWith(
    fetch(event.request)
      .then((resp) => {
        if (!resp || resp.status >= 400) return resp;
        return resp
          .clone()
          .text()
          .then((body) => {
            const clone = new Response(body, resp);
            caches
              .open(API_CACHE)
              .then((cache) => cache.put(event.request, clone));
            return resp;
          });
      })
      .catch(() => caches.match(event.request))
  );
});
