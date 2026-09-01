// Register the service worker for offline/installable support. No-op on
// browsers without SW support; only takes effect over HTTPS or localhost.
//
// Kept as its own file (not an inline <script> in index.html) so the
// dashboard can run under a strict script-src 'self' Content-Security-Policy
// with no 'unsafe-inline' - see securityHeaders in cmd/backend/backend.go.
if ("serviceWorker" in navigator) {
  window.addEventListener("load", function () {
    navigator.serviceWorker.register("/sw.js").catch(function (err) {
      console.debug("Service worker registration failed:", err);
    });
  });
}
