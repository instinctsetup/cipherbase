const $ = (s) => document.querySelector(s);
async function api(path, opts) { return (await fetch(path, opts)).json(); }
async function tick() {
  const s = await api("/api/stats");
  $("#s-puts").textContent = s.puts.toLocaleString();
  $("#s-gets").textContent = s.gets.toLocaleString();
  $("#s-flushes").textContent = s.flushes;
  $("#s-compactions").textContent = s.compactions;
  $("#s-bloom").textContent = s.bloom_skips.toLocaleString();
  $("#l0").textContent = s.levels.l0_files ? `${s.levels.l0_files} file(s), ${s.levels.l0_entries.toLocaleString()} entries` : "empty";
  $("#l1").textContent = s.levels.l1_files ? `${s.levels.l1_files} file(s), ${s.levels.l1_entries.toLocaleString()} entries` : "empty";
}
$("#put").onclick = async () => { await api("/api/put", { method: "POST", body: JSON.stringify({ key: $("#k").value, value: $("#v").value }) }); $("#result").textContent = `put ${$("#k").value} ok`; tick(); };
$("#get").onclick = async () => { const r = await api("/api/get?key=" + encodeURIComponent($("#k").value)); $("#result").textContent = r.found ? `${$("#k").value} = ${r.value}` : "not found"; tick(); };
$("#del").onclick = async () => { await api("/api/delete?key=" + encodeURIComponent($("#k").value), { method: "DELETE" }); $("#result").textContent = `deleted ${$("#k").value}`; tick(); };
$("#scan").onclick = async () => { const r = await api("/api/scan"); $("#scan-out").innerHTML = r.entries.map(e => `<div>${e.key} = ${e.value}</div>`).join("") || "empty"; };
$("#load").onclick = async () => { $("#load").disabled = true; await api("/api/load", { method: "POST", body: "{}" }); setTimeout(() => $("#load").disabled = false, 15000); };
$("#flush").onclick = async () => { await api("/api/flush", { method: "POST" }); tick(); };
$("#compact").onclick = async () => { await api("/api/compact", { method: "POST" }); tick(); };
tick(); setInterval(tick, 1000);
