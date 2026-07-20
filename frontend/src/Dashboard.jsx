import { useCallback, useEffect, useMemo, useRef, useState, useTransition } from "react";
import Layout from "./ui/Layout";
import { api, getSession, clearSession } from "./api";

/* ── tiny helpers ─────────────────────────────────────────── */
const fmt = (n) => Number(n ?? 0).toLocaleString("vi-VN") + " ₫";
const PRESETS = [10000, 50000, 100000, 500000];

function CopyBtn({ text }) {
  const [copied, setCopied] = useState(false);
  const copy = () => {
    navigator.clipboard?.writeText(text).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  };
  return (
    <button
      onClick={copy}
      title="Copy"
      className="ml-1.5 rounded-md border border-slate-200 px-2 py-0.5 text-xs text-slate-500 hover:bg-slate-100 transition-colors"
    >
      {copied ? "✓" : "copy"}
    </button>
  );
}

function StatRow({ label, value, highlight }) {
  return (
    <div className="flex items-center justify-between py-2 border-b border-slate-100 last:border-0">
      <span className="text-xs text-slate-500">{label}</span>
      <span className={`text-sm font-semibold ${highlight ? "text-blue-700" : "text-slate-900"}`}>{value}</span>
    </div>
  );
}

/* ── main component ───────────────────────────────────────── */
export default function Dashboard({ onLogout, onGoAdmin }) {
  const [me, setMe]           = useState(null);
  const [recipient, setRecipient] = useState("");
  const [recipientName, setRecipientName] = useState(null); // null=idle, ""=not found, "name"=found
  const [amount, setAmount]   = useState("");
  const [notifs, setNotifs]   = useState([]);
  const [wsStatus, setWsStatus] = useState("disconnected");
  const [msg, setMsg]         = useState("");
  const [err, setErr]         = useState("");
  const [isPending, startTransition] = useTransition();
  const recipientTimer = useRef(null);

  const session = getSession();

  const wsUrl = useMemo(() => {
    const scheme = window.location.protocol === "https:" ? "wss" : "ws";
    return `${scheme}://${window.location.host}/ws?session=${encodeURIComponent(session || "")}`;
  }, [session]);

  const load = async () => {
    const m = await api.me();
    setMe(m);
    const n = await api.notifications().catch(() => ({}));
    setNotifs(Array.isArray(n.notifications) ? n.notifications : []);
  };

  useEffect(() => {
    load().catch(console.error);
    if (!session) return;
    let ws;
    try { ws = new WebSocket(wsUrl); } catch { return; }
    ws.onopen    = () => setWsStatus("connected");
    ws.onclose   = () => setWsStatus("disconnected");
    ws.onerror   = () => setWsStatus("error");
    ws.onmessage = (e) => {
      // WS push signals a new transfer event; re-fetch the canonical
      // notification list from the REST API so the UI always shows
      // DB-backed items with the correct shape (id, message, is_read, created_at).
      // The server now sends {"event": {...TransferCompleted...}} — parse it to
      // optimistically update the balance before the REST refetch completes.
      try {
        const { event } = JSON.parse(e.data);
        if (event?.sender_id || event?.receiver_id) {
          setMe((prev) => {
            if (!prev) return prev;
            const myId = prev.id;
            if (event.sender_id === myId)   return { ...prev, balance: event.sender_balance };
            if (event.receiver_id === myId) return { ...prev, balance: event.receiver_balance };
            return prev;
          });
        }
      } catch {}
      load().catch(console.error);
    };
    return () => { try { ws.close(); } catch {} };
  }, [wsUrl, session]);

  /* debounced recipient lookup */
  const onRecipientChange = (v) => {
    setRecipient(v);
    setRecipientName(null);
    clearTimeout(recipientTimer.current);
    if (!v.trim()) return;
    recipientTimer.current = setTimeout(async () => {
      try {
        const r = await api.lookupAccount(v.trim());
        setRecipientName(r.username || "");
      } catch {
        setRecipientName("");
      }
    }, 400);
  };

  const ackNotif = useCallback(async (id) => {
    // Optimistically mark as read in local state so the UI responds instantly.
    setNotifs((prev) => prev.map((n) => n.id === id ? { ...n, is_read: true } : n));
    try {
      await api.ackNotification(id);
    } catch {
      // On failure revert: re-fetch from server to restore correct state.
      load().catch(console.error);
    }
  }, []);

  const doTransfer = () => {
    setErr(""); setMsg("");
    if (!recipient.trim()) { setErr("Enter phone or account number"); return; }
    const num = Number(amount);
    if (!amount || isNaN(num) || num <= 0) { setErr("Enter a valid amount"); return; }
    if (!Number.isInteger(num))            { setErr("Amount must be a whole number"); return; }
    startTransition(async () => {
      try {
        const r = await api.transfer(recipient.trim(), num);
        const toLabel = recipientName || recipient.trim();
        setMsg(`Sent ${fmt(r.amount)} → ${toLabel}`);
        setRecipient(""); setRecipientName(null); setAmount("");
        await load();
      } catch (e) { setErr(e.message); }
    });
  };

  const wsBadge = wsStatus === "connected"
    ? "bg-emerald-50 text-emerald-700 border-emerald-200"
    : wsStatus === "error"
    ? "bg-red-50 text-red-700 border-red-200"
    : "bg-slate-50 text-slate-500 border-slate-200";

  const unread = notifs.filter((n) => !n.is_read).length;

  return (
    <Layout user={me?.username} env="LAB" onLogout={() => { clearSession(); onLogout?.(); }} onGoAdmin={onGoAdmin} activePage="dashboard">
      <div className="space-y-5">

        {/* ── Row 1: account summary ─────────────────────────── */}
        <div className="grid gap-5 sm:grid-cols-2 xl:grid-cols-4">

          {/* balance hero */}
          <div className="sm:col-span-2 rounded-2xl bg-gradient-to-br from-blue-600 to-blue-700 p-6 text-white shadow-sm">
            <div className="flex items-center justify-between mb-4">
              <span className="text-xs font-semibold uppercase tracking-wider opacity-80">Available Balance</span>
              <span className={`rounded-full border px-2.5 py-1 text-xs font-semibold ${wsBadge}`}>
                {wsStatus === "connected" ? "● Live" : wsStatus === "error" ? "✕ WS error" : "○ Offline"}
              </span>
            </div>
            <div className="text-4xl font-bold tracking-tight">{fmt(me?.balance)}</div>
            <div className="mt-4 grid grid-cols-2 gap-3 text-xs opacity-80">
              <div>
                <div className="opacity-70">Account</div>
                <div className="mt-0.5 font-mono font-semibold">
                  {me?.account_number || "—"}
                  {me?.account_number && <CopyBtn text={me.account_number} />}
                </div>
              </div>
              <div>
                <div className="opacity-70">Phone</div>
                <div className="mt-0.5 font-semibold">{me?.phone || "—"}</div>
              </div>
            </div>
          </div>

          {/* account details */}
          <div className="rounded-2xl border bg-white p-5 shadow-xs">
            <div className="text-xs font-semibold text-slate-500 uppercase tracking-wider mb-3">Profile</div>
            <StatRow label="Username" value={me?.username || "—"} />
            <StatRow label="Phone" value={me?.phone || "—"} />
            <StatRow label="Account No." value={
              <span className="font-mono text-xs">
                {me?.account_number || "—"}
                {me?.account_number && <CopyBtn text={me.account_number} />}
              </span>
            } />
          </div>

          {/* notification summary */}
          <div className="rounded-2xl border bg-white p-5 shadow-xs">
            <div className="flex items-center justify-between mb-3">
              <div className="text-xs font-semibold text-slate-500 uppercase tracking-wider">Notifications</div>
              {unread > 0 && (
                <span className="rounded-full bg-red-500 px-2 py-0.5 text-xs font-bold text-white">{unread}</span>
              )}
            </div>
            <div className="text-3xl font-bold text-slate-900">{notifs.length}</div>
            <div className="text-xs text-slate-500 mt-1">{unread} unread</div>
            <div className="mt-3 text-xs text-slate-400">
              {wsStatus === "connected"
                ? "Real-time updates active via WebSocket"
                : "Connect to receive live updates"}
            </div>
          </div>
        </div>

        {/* ── Row 2: transfer ────────────────────────────────── */}
        <div className="rounded-2xl border bg-white p-6 shadow-xs">
          <div className="mb-4">
            <div className="text-sm font-semibold text-slate-900">Transfer Money</div>
            <div className="text-xs text-slate-500 mt-0.5">Enter phone number or 12-digit account number</div>
          </div>

          <div className="grid gap-4 sm:grid-cols-2">
            {/* recipient */}
            <div className="space-y-2">
              <label className="text-xs font-semibold text-slate-600">Recipient</label>
              <input
                className="w-full rounded-xl border px-4 py-3 text-sm outline-none focus:ring-2 focus:ring-blue-500 transition-shadow"
                placeholder="Phone or account number"
                inputMode="numeric"
                value={recipient}
                onChange={(e) => onRecipientChange(e.target.value)}
              />
              {/* lookup result */}
              {recipientName === null && recipient.trim() && (
                <div className="rounded-lg bg-slate-50 border px-3 py-2 text-xs text-slate-400">Looking up…</div>
              )}
              {recipientName === "" && (
                <div className="rounded-lg bg-red-50 border border-red-200 px-3 py-2 text-xs text-red-600">Recipient not found</div>
              )}
              {recipientName && (
                <div className="rounded-lg bg-emerald-50 border border-emerald-200 px-3 py-2 text-xs font-semibold text-emerald-700">
                  ✓ {recipientName}
                </div>
              )}
            </div>

            {/* amount */}
            <div className="space-y-2">
              <label className="text-xs font-semibold text-slate-600">Amount</label>
              <input
                className="w-full rounded-xl border px-4 py-3 text-sm outline-none focus:ring-2 focus:ring-blue-500 transition-shadow"
                placeholder="e.g. 50000"
                inputMode="numeric"
                value={amount}
                onChange={(e) => setAmount(e.target.value)}
                onKeyDown={(e) => e.key === "Enter" && doTransfer()}
              />
              {/* presets */}
              <div className="flex gap-2 flex-wrap">
                {PRESETS.map((p) => (
                  <button
                    key={p}
                    onClick={() => setAmount(String(p))}
                    className={`rounded-lg border px-2.5 py-1 text-xs font-semibold transition-colors ${
                      amount === String(p)
                        ? "bg-blue-600 border-blue-600 text-white"
                        : "text-slate-600 hover:bg-slate-50"
                    }`}
                  >
                    {(p / 1000).toFixed(0)}k
                  </button>
                ))}
              </div>
            </div>
          </div>

          {/* actions */}
          <div className="mt-4 flex gap-3 items-center">
            <button
              onClick={doTransfer}
              disabled={isPending || !recipient.trim() || !amount}
              className="rounded-xl bg-blue-600 px-6 py-3 text-sm font-semibold text-white hover:bg-blue-700 disabled:opacity-50 transition-colors"
            >
              {isPending ? "Sending…" : "Send Money"}
            </button>
            <button
              onClick={() => load().catch(() => {})}
              className="rounded-xl border px-4 py-3 text-xs font-semibold text-slate-600 hover:bg-slate-50 transition-colors"
            >
              Refresh
            </button>
            {amount && <span className="text-sm text-slate-500">= {fmt(Number(amount) || 0)}</span>}
          </div>

          {msg && (
            <div className="mt-3 rounded-xl border border-emerald-200 bg-emerald-50 px-4 py-3 text-sm font-semibold text-emerald-800">
              ✓ {msg}
            </div>
          )}
          {err && (
            <div className="mt-3 rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">
              {err}
            </div>
          )}
        </div>

        {/* ── Row 3: notifications feed ───────────────────────── */}
        <div className="rounded-2xl border bg-white shadow-xs">
          <div className="flex items-center justify-between px-6 py-4 border-b">
            <div>
              <div className="text-sm font-semibold text-slate-900">Activity Feed</div>
              <div className="text-xs text-slate-500 mt-0.5">Transfer notifications via WebSocket</div>
            </div>
            <span className={`rounded-full border px-2.5 py-1 text-xs font-semibold ${wsBadge}`}>
              {wsStatus}
            </span>
          </div>

          {notifs.length === 0 ? (
            <div className="px-6 py-10 text-center text-sm text-slate-400">
              No activity yet — make a transfer to see notifications.
            </div>
          ) : (
            <div className="divide-y">
              {notifs.slice(0, 20).map((n, idx) => (
                <div key={n.id ?? idx} className={`flex items-start gap-3 px-6 py-4 transition-colors ${!n.is_read ? "bg-blue-50/40" : ""}`}>
                  <div className={`mt-0.5 h-8 w-8 shrink-0 rounded-full grid place-items-center text-sm font-bold ${
                    n.message?.includes("nhận") || n.message?.includes("received")
                      ? "bg-emerald-100 text-emerald-700"
                      : "bg-blue-100 text-blue-700"
                  }`}>
                    {n.message?.includes("nhận") || n.message?.includes("received") ? "↓" : "↑"}
                  </div>
                  <div className="flex-1 min-w-0">
                    <div className="text-sm text-slate-800">{n.message ?? JSON.stringify(n)}</div>
                    {n.created_at && (
                      <div className="mt-0.5 text-xs text-slate-400">
                        {new Date(n.created_at).toLocaleString()}
                      </div>
                    )}
                  </div>
                  {!n.is_read && (
                    <button
                      onClick={() => ackNotif(n.id)}
                      title="Mark as read"
                      className="mt-0.5 shrink-0 rounded-full border border-blue-200 bg-blue-50 px-2 py-0.5 text-xs font-semibold text-blue-600 hover:bg-blue-100 transition-colors"
                    >
                      ✓ ack
                    </button>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>

      </div>
    </Layout>
  );
}
