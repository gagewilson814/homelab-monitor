const form = document.getElementById("login-form");
const passwordEl = document.getElementById("password");
const errorEl = document.getElementById("login-error");

form.addEventListener("submit", async (e) => {
  e.preventDefault();
  errorEl.textContent = "";

  try {
    const res = await fetch("/api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ password: passwordEl.value }),
    });

    if (!res.ok) {
      errorEl.textContent = "Invalid password";
      return;
    }

    window.location.href = "/";
  } catch (err) {
    errorEl.textContent = `error: ${err.message}`;
  }
});
