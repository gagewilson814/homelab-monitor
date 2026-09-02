const form = document.getElementById("login-form");
const usernameEl = document.getElementById("username");
const passwordEl = document.getElementById("password");
const errorEl = document.getElementById("login-error");
const loggedInEl = document.getElementById("logged-in");
const loggedInUserEl = document.getElementById("logged-in-user");
const loggedInLogoutBtn = document.getElementById("logged-in-logout");

// If a valid session cookie already exists, offer logout instead of a
// second login form. /api/me 401s when it doesn't.
(async function showLoggedInState() {
  try {
    const res = await fetch("/api/me");
    if (!res.ok) return;
    const me = await res.json();
    loggedInUserEl.textContent = me.username;
    form.hidden = true;
    loggedInEl.hidden = false;
  } catch {
    // Backend unreachable: leave the form as-is; its own submit will report
    // the failure if the user tries anyway.
  }
})();

loggedInLogoutBtn.addEventListener("click", async () => {
  await fetch("/api/logout", { method: "POST" });
  window.location.reload();
});

form.addEventListener("submit", async (e) => {
  e.preventDefault();
  errorEl.textContent = "";

  try {
    const res = await fetch("/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username: usernameEl.value, password: passwordEl.value }),
    });

    if (!res.ok) {
      errorEl.textContent = "Invalid username or password";
      return;
    }

    window.location.href = "/";
  } catch (err) {
    errorEl.textContent = `error: ${err.message}`;
  }
});
