const $ = (id) => document.getElementById(id);
const fields = ["port", "token", "allowCurrentTab", "autoconnect"];

async function load() {
  const s = { port: 9223, token: "", allowCurrentTab: false, autoconnect: false, ...(await chrome.storage.local.get(fields)) };
  $("port").value = s.port; $("token").value = s.token;
  $("allowCurrentTab").checked = s.allowCurrentTab; $("autoconnect").checked = s.autoconnect;
  chrome.runtime.sendMessage({ type: "status" }, show);
}
async function save() {
  await chrome.storage.local.set({
    port: Number($("port").value) || 9223, token: $("token").value.trim(),
    allowCurrentTab: $("allowCurrentTab").checked, autoconnect: $("autoconnect").checked,
  });
}
function show(st) {
  if (!st) return;
  const el = $("status");
  el.className = st.state;
  el.textContent = st.state + (st.detail ? " · " + st.detail : "") + (st.owned && st.owned.length ? ` · ${st.owned.length} tab` : "");
}
$("connect").onclick = async () => { await save(); chrome.runtime.sendMessage({ type: "connect" }, show); };
$("disconnect").onclick = () => chrome.runtime.sendMessage({ type: "disconnect" }, show);
for (const f of fields) $(f).addEventListener("change", save);
chrome.runtime.onMessage.addListener((m) => { if (m.type === "status") show(m); });
load();
