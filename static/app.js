const base = window.BASE_PATH || '';
const $ = (id) => document.getElementById(id);

const DAYS = ['Sun', 'Mon', 'Tue', 'Wed', 'Thu', 'Fri', 'Sat'];
const MODE_LABEL = { AUTO: 'Auto', COOL: 'Cool', HEAT: 'Heat', OFF: 'Off' };
const STATE_LABEL = { heating: 'Heating', cooling: 'Cooling', idle: 'Idle', off: 'Off', offline: 'Offline' };

let snap = null;
let hasDB = false;
let hours = 24;
let editingId = null;
// Setpoints being adjusted, by zone id, so a refresh doesn't snap them back
const pending = {};

async function api(path, options = {}) {
    const res = await fetch(base + path, {
        ...options,
        headers: { 'Content-Type': 'application/json', ...(options.headers || {}) },
    });
    let body = {};
    try { body = await res.json(); } catch (_) { /* non-JSON error page */ }
    if (!res.ok) throw new Error(body.error || `HTTP ${res.status}`);
    return body;
}

const current = () => (snap ? snap.thermostats.find((t) => t.id === $('thSelect').value) : null);
const deg = (v) => (v == null ? '—' : `${Math.round(v * 10) / 10}°`);

function setMsg(el, text, ok) {
    el.textContent = text;
    el.className = 'msg ' + (ok === undefined ? '' : ok ? 'ok' : 'bad');
}

function el(tag, attrs = {}, ...children) {
    const e = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
        if (k === 'class') e.className = v;
        else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
        else e.setAttribute(k, v);
    }
    children.forEach((c) => e.append(c));
    return e;
}

function option(value, text) {
    const o = document.createElement('option');
    o.value = value;
    o.textContent = text;
    return o;
}

function cell(text, cls) {
    const td = document.createElement('td');
    td.textContent = text;
    if (cls) td.className = cls;
    return td;
}

function fmtWhen(iso) {
    if (!iso) return '—';
    const d = new Date(iso);
    const today = new Date().toDateString() === d.toDateString();
    return today
        ? 'Today ' + d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
        : d.toLocaleString([], { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
}

function fmtMinutes(m) {
    m = Math.round(m);
    return m < 60 ? `${m} min` : `${Math.floor(m / 60)} h ${m % 60} min`;
}

function fmtDays(mask) {
    if (mask === 127) return 'Every day';
    if (mask === 62) return 'Weekdays';
    if (mask === 65) return 'Weekends';
    return DAYS.filter((_, i) => mask & (1 << i)).join(' ');
}

async function command(body, okText) {
    const t = current();
    if (!t) return;
    setMsg($('ctlMsg'), 'Sending…');
    try {
        await api(`/api/thermostats/${t.id}/command`, { method: 'POST', body: JSON.stringify(body) });
        setMsg($('ctlMsg'), (okText || 'Sent') + '. The thermostat shows it within a few seconds.', true);
        // The service re-reads the cloud 5 s after a change
        setTimeout(refresh, 7000);
        if (hasDB) setTimeout(loadCommands, 1000);
    } catch (e) {
        setMsg($('ctlMsg'), e.message, false);
        refresh();
    }
}

// ---- Status and zones ----

async function refresh() {
    let data;
    try {
        data = await api('/api/thermostats');
    } catch (e) {
        setMsg($('ctlMsg'), e.message, false);
        return;
    }
    hasDB = data.database;
    snap = data.snapshot;
    const sel = $('thSelect');
    const ids = snap.thermostats.map((t) => t.id).join();
    const listChanged = sel.dataset.ids !== ids;
    if (listChanged) {
        const keep = sel.value;
        sel.replaceChildren(...snap.thermostats.map((t) => option(t.id, t.name)));
        if (snap.thermostats.some((t) => t.id === keep)) sel.value = keep;
        sel.dataset.ids = ids;
        sel.classList.toggle('hidden', snap.thermostats.length < 2);
    }
    $('errBanner').textContent = snap.error ? '⚠️ ' + snap.error : '';
    $('errBanner').classList.toggle('hidden', !snap.error);
    ['historyPanel', 'schedPanel', 'logPanel'].forEach((id) => $(id).classList.toggle('hidden', !hasDB));
    $('noDbPanel').classList.toggle('hidden', hasDB);
    if (listChanged) selectionChanged();
    else render();
}

function render() {
    const t = current();
    if (!t) {
        $('stState').textContent = snap && snap.updated ? 'No thermostats found' : 'Waiting for the first reading…';
        $('zones').replaceChildren();
        return;
    }
    $('seenNote').textContent = snap.updated ? `${t.model || 'thermostat'} · read ${fmtWhen(snap.updated)}` : '';
    $('stState').textContent = t.online ? t.status || STATE_LABEL[t.run_state] : 'Offline';
    $('stState').className = 'v ' + (t.online ? t.run_state : 'bad');
    $('stHum').textContent = t.humidity == null ? '—' : `${Math.round(t.humidity)}%`;
    $('stOut').textContent = deg(t.outdoor);

    const zones = $('zones');
    if (zones.dataset.for !== t.id || zones.children.length !== t.zones.length) {
        zones.replaceChildren(...t.zones.map((z) => buildZone(t, z)));
        zones.dataset.for = t.id;
    }
    t.zones.forEach((z) => updateZone(t, z));
    renderSettings(t);
}

function buildZone(t, z) {
    const sp = (kind, label) => el('div', {},
        el('div', { class: 'sp-label' }, label),
        el('div', { class: `sp ${kind}` },
            el('button', { class: 'btn secondary', 'aria-label': `${label} down`, onclick: () => nudge(z.id, kind, -1) }, '−'),
            el('span', { class: 'val', 'data-k': kind }, '—'),
            el('button', { class: 'btn secondary', 'aria-label': `${label} up`, onclick: () => nudge(z.id, kind, 1) }, '+')));
    const mode = el('select', { 'data-k': 'mode', onchange: (e) => command({ action: 'mode', zone: z.id, value: e.target.value }, 'Mode changed') },
        ...z.modes.map((m) => option(m, MODE_LABEL[m] || m)));
    return el('div', { class: 'zone', 'data-zone': z.id },
        el('div', { class: 'dial', 'data-k': 'dial' },
            el('div', { class: 'temp', 'data-k': 'temp' }, '—'),
            el('div', { class: 'zname', 'data-k': 'zstatus' }, '')),
        el('div', { class: 'zone-ctl' },
            el('div', { class: 'row' },
                el('label', {}, 'Mode', mode),
                el('div', { 'data-k': 'heatWrap' }, sp('heat', 'Heat to')),
                el('div', { 'data-k': 'coolWrap' }, sp('cool', 'Cool to'))),
            el('div', { class: 'row' },
                el('button', { class: 'btn secondary', 'data-k': 'hold', onclick: () => toggleHold(z.id) }, 'Hold'),
                el('span', { class: 'muted', 'data-k': 'runmode' }, ''))));
}

function updateZone(t, z) {
    const root = document.querySelector(`.zone[data-zone="${CSS.escape(z.id)}"]`);
    if (!root) return;
    const k = (name) => root.querySelector(`[data-k="${name}"]`);
    k('temp').textContent = deg(z.temp);
    k('zstatus').textContent = (t.zones.length > 1 ? z.name + ' · ' : '') + z.status;
    k('dial').className = 'dial ' + (t.online ? t.run_state : '');
    if (document.activeElement !== k('mode')) k('mode').value = z.mode;
    const p = pending[z.id] || {};
    k('heat').textContent = deg(p.heat ?? z.heat);
    k('cool').textContent = deg(p.cool ?? z.cool);
    k('heatWrap').classList.toggle('hidden', !['HEAT', 'AUTO'].includes(z.mode));
    k('coolWrap').classList.toggle('hidden', !['COOL', 'AUTO'].includes(z.mode));
    k('hold').classList.toggle('hidden', !z.can_hold);
    k('hold').textContent = z.hold ? '📅 Follow schedule' : '✋ Hold';
    k('runmode').textContent = z.can_hold ? `Now: ${z.run_mode}` : 'Scheduling is off on the thermostat';
}

// nudge moves a setpoint one step and sends it once the clicking stops.
function nudge(zoneId, kind, dir) {
    const t = current();
    const z = t && t.zones.find((x) => x.id === zoneId);
    if (!z) return;
    const p = (pending[zoneId] = pending[zoneId] || {});
    const step = t.unit === 'C' ? 0.5 : 1;
    let v = (p[kind] ?? z[kind] ?? (kind === 'heat' ? 68 : 76)) + dir * step;
    v = Math.min(Math.max(v, t.heat_min), t.cool_max);
    p[kind] = v;
    // Keep the other the deadband away, as the thermostat will
    if (kind === 'heat') {
        const c = p.cool ?? z.cool;
        if (c != null && c - v < t.deadband) p.cool = v + t.deadband;
    } else {
        const h = p.heat ?? z.heat;
        if (h != null && v - h < t.deadband) p.heat = v - t.deadband;
    }
    updateZone(t, z);
    clearTimeout(p.timer);
    p.timer = setTimeout(() => {
        delete pending[zoneId];
        const body = { action: 'setpoints', zone: zoneId };
        if (p.heat != null) body.heat = p.heat;
        if (p.cool != null) body.cool = p.cool;
        command(body, 'Setpoint changed');
    }, 1500);
}

function toggleHold(zoneId) {
    const z = current().zones.find((x) => x.id === zoneId);
    if (z.hold) command({ action: 'schedule', zone: zoneId }, 'Following the schedule');
    else command({ action: 'hold', zone: zoneId }, 'Holding');
}

function renderSettings(t) {
    const list = [];
    t.zones.forEach((z) => z.settings.forEach((s) => list.push({ s, zone: z.id, prefix: t.zones.length > 1 ? z.name + ': ' : '' })));
    t.settings.forEach((s) => list.push({ s, zone: '', prefix: '' }));
    $('settingsPanel').classList.toggle('hidden', list.length === 0);
    const box = $('settings');
    const key = (x) => x.zone + '/' + x.s.type;
    if (box.dataset.keys !== list.map(key).join()) {
        box.replaceChildren(...list.map((x) => {
            const sel = el('select', { 'data-key': key(x), onchange: (e) => command({ action: 'setting', zone: x.zone, setting: x.s.type, value: e.target.value }, x.s.title + ' changed') },
                ...x.s.options.map((o) => option(o.value, o.label)));
            return el('label', {}, x.prefix + x.s.title, sel);
        }));
        box.dataset.keys = list.map(key).join();
    }
    list.forEach((x) => {
        const sel = box.querySelector(`[data-key="${CSS.escape(key(x))}"]`);
        if (sel && document.activeElement !== sel) sel.value = x.s.value;
    });
}

function selectionChanged() {
    $('zones').dataset.for = '';
    render();
    loadCloudHistory();
    if (!hasDB) return;
    loadHistory();
    loadSchedules();
    loadCommands();
}

// ---- The thermostat's own run time (from the cloud) ----

let period = 'daily';

// Heat in reds and cool in blues, paler for higher stages; anything else grey
function seriesColor(s) {
    const st = String(s.state || s.name || '').toLowerCase();
    const stage = Number((st.match(/stage\s*_?(\d)/) || [])[1] || 1);
    const opacity = [1, 1, 0.55, 0.3][stage] ?? 1;
    const color = st.includes('heat') ? 'var(--heat)' : st.includes('cool') ? 'var(--cool)' : 'var(--text-muted)';
    return { color, opacity };
}

const hrs = (v) => (v == null || v === '' ? '-' : typeof v === 'number' ? (Math.round(v * 10) / 10).toString() : String(v));

async function loadCloudHistory() {
    const t = current();
    if (!t) return;
    let data;
    try {
        data = await api(`/api/thermostats/${t.id}/runtime?period=${period}`);
    } catch (e) {
        ['cloudLegend', 'cloudBars', 'cloudAxis', 'cloudTable'].forEach((id) => $(id).replaceChildren());
        setMsg($('cloudMsg'), e.message, false);
        return;
    }
    setMsg($('cloudMsg'), '');
    const cats = data.categories || [];
    const series = (data.series || []).filter((s) => Array.isArray(s.data));
    // Stacked series are the chart; skip the ones that never ran
    const stacked = series.filter((s) => s.stack && s.data.some((v) => Number(v) > 0));
    const total = (i) => stacked.reduce((sum, s) => sum + (Number(s.data[i]) || 0), 0);
    const top = Math.max(1, ...cats.map((_, i) => total(i)));

    $('cloudLegend').replaceChildren(...(stacked.length ? stacked.map((s) => {
        const c = seriesColor(s);
        return el('span', { class: 'legend' }, el('i', { style: `background:${c.color};opacity:${c.opacity}` }), s.name);
    }) : [el('span', {}, 'No run time in this period.')]));

    $('cloudBars').replaceChildren(...cats.map((cat, i) => {
        const col = el('div', { class: 'col', title: `${cat}: ` + stacked.map((s) => `${s.name} ${hrs(Number(s.data[i]) || 0)} h`).join(', ') });
        // Drawn top to bottom, so the last series sits on the axis
        const segs = stacked.filter((s) => Number(s.data[i]) > 0).reverse();
        segs.forEach((s, k) => {
            const c = seriesColor(s);
            col.append(el('div', { class: 'seg stack' + (k === 0 ? ' top' : ''), style: `height:${(Number(s.data[i]) / top) * 100}%;background:${c.color};opacity:${c.opacity}` }));
        });
        return col;
    }));
    $('cloudAxis').replaceChildren(...cats.map((c) => el('span', { title: c }, String(c).slice(0, 3))));

    // Table: the run time, any other per-day series, and any other list in
    // the reply that lines up with the days (the website's table rows)
    const rows = stacked.map((s) => [s.name + ' (h)', s.data]);
    series.filter((s) => !s.stack).forEach((s) => rows.push([s.name, s.data]));
    for (const [k, v] of Object.entries(data)) {
        if (k !== 'categories' && k !== 'series' && Array.isArray(v) && v.length === cats.length && v.every((x) => typeof x !== 'object' || x === null)) {
            rows.push([k.replace(/_/g, ' ').replace(/^./, (c) => c.toUpperCase()), v]);
        }
    }
    $('cloudTable').replaceChildren(
        el('thead', {}, el('tr', {}, el('th', {}, ''), ...cats.map((c) => el('th', {}, String(c).slice(0, 3))))),
        el('tbody', {}, ...rows.map(([name, vals]) => el('tr', {}, el('td', {}, name), ...cats.map((_, i) => el('td', {}, hrs(vals[i])))))),
    );
}

// ---- History ----

async function loadHistory() {
    const t = current();
    if (!t || !hasDB) return;
    const z = t.zones[0];
    $('histZone').textContent = t.zones.length > 1 ? z.name : '';
    let data;
    try {
        data = await api(`/api/thermostats/${t.id}/history?hours=${hours}&zone=${encodeURIComponent(z.id)}`);
    } catch (e) {
        $('tempChart').replaceChildren();
        return;
    }
    drawTemps(data.readings);
    drawRuntime(data.runtime);
    const today = data.runtime[data.runtime.length - 1];
    $('stHeatToday').textContent = today ? fmtMinutes(today.heating) : '—';
    $('stCoolToday').textContent = today ? fmtMinutes(today.cooling) : '—';
}

function drawTemps(rs) {
    const svg = $('tempChart');
    const W = svg.clientWidth || 900, H = 200, L = 34, R = 8, T = 10, B = 22;
    svg.setAttribute('viewBox', `0 0 ${W} ${H}`);
    svg.replaceChildren();
    const ns = 'http://www.w3.org/2000/svg';
    const add = (tag, attrs, text) => {
        const e = document.createElementNS(ns, tag);
        for (const [k, v] of Object.entries(attrs)) e.setAttribute(k, v);
        if (text != null) e.textContent = text;
        svg.appendChild(e);
        return e;
    };
    if (!rs.length) {
        add('text', { x: W / 2, y: H / 2, 'text-anchor': 'middle' }, 'No readings yet');
        return;
    }
    const series = [['temp', 'var(--text-main)', ''], ['heat', 'var(--heat)', '5 4'], ['cool', 'var(--cool)', '5 4'], ['outdoor', 'var(--warn)', '2 3']];
    const vals = rs.flatMap((r) => series.map(([k]) => r[k]).filter((v) => v != null));
    let lo = Math.floor(Math.min(...vals) / 5) * 5, hi = Math.ceil(Math.max(...vals) / 5) * 5;
    if (hi - lo < 10) hi = lo + 10;
    const t0 = new Date(rs[0].at).getTime(), t1 = Math.max(new Date(rs[rs.length - 1].at).getTime(), t0 + 1);
    const x = (at) => L + ((new Date(at).getTime() - t0) / (t1 - t0)) * (W - L - R);
    const y = (v) => T + (1 - (v - lo) / (hi - lo)) * (H - T - B);
    for (let v = lo; v <= hi; v += hi - lo > 30 ? 10 : 5) {
        add('line', { class: 'grid', x1: L, x2: W - R, y1: y(v), y2: y(v) });
        add('text', { x: L - 4, y: y(v) + 4, 'text-anchor': 'end' }, `${v}°`);
    }
    const ticks = 6;
    for (let i = 0; i <= ticks; i++) {
        const at = t0 + ((t1 - t0) * i) / ticks;
        const d = new Date(at);
        const label = hours <= 24 ? d.toLocaleTimeString([], { hour: 'numeric' }) : d.toLocaleDateString([], { weekday: 'short', hour: 'numeric' });
        add('text', { x: x(at), y: H - 6, 'text-anchor': i === 0 ? 'start' : i === ticks ? 'end' : 'middle' }, label);
    }
    // Shade when the system ran
    rs.forEach((r, i) => {
        if (r.run_state !== 'heating' && r.run_state !== 'cooling' || i + 1 >= rs.length) return;
        const x1 = x(r.at), x2 = x(rs[i + 1].at);
        add('rect', { x: x1, y: T, width: Math.max(x2 - x1, 1), height: H - T - B, fill: `var(--${r.run_state === 'heating' ? 'heat' : 'cool'})`, opacity: 0.08 });
    });
    for (const [k, color, dash] of series) {
        let d = '';
        let gap = true;
        rs.forEach((r, i) => {
            if (r[k] == null || (i > 0 && new Date(r.at) - new Date(rs[i - 1].at) > 15 * 60000)) gap = true;
            if (r[k] == null) return;
            d += `${gap ? 'M' : 'L'}${x(r.at).toFixed(1)},${y(r[k]).toFixed(1)}`;
            gap = false;
        });
        if (d) add('path', { d, fill: 'none', stroke: color, 'stroke-width': k === 'temp' ? 2.2 : 1.5, 'stroke-dasharray': dash });
    }
}

function drawRuntime(days) {
    const maxMin = Math.max(60, ...days.map((d) => d.heating + d.cooling));
    $('runBars').replaceChildren(...days.map((d) => {
        const col = el('div', { class: 'col', title: `${d.date}: heating ${fmtMinutes(d.heating)}, cooling ${fmtMinutes(d.cooling)}` });
        col.append(el('div', { class: 'seg cool', style: `height:${(d.cooling / maxMin) * 100}%` }),
            el('div', { class: 'seg heat', style: `height:${(d.heating / maxMin) * 100}%` }));
        return col;
    }));
    $('runAxis').replaceChildren(...days.map((d, i) => el('span', {},
        i % 2 === days.length % 2 ? '' : new Date(d.date + 'T00:00').toLocaleDateString([], { month: 'numeric', day: 'numeric' }))));
}

// ---- Schedules ----

function describeChange(sc) {
    const parts = [];
    if (sc.mode) parts.push(MODE_LABEL[sc.mode] || sc.mode);
    if (sc.heat != null) parts.push(`heat ${sc.heat}°`);
    if (sc.cool != null) parts.push(`cool ${sc.cool}°`);
    if (sc.hold === 'hold') parts.push('hold');
    if (sc.hold === 'schedule') parts.push('follow schedule');
    return parts.join(', ');
}

async function loadSchedules() {
    const t = current();
    if (!t || !hasDB) return;
    let data;
    try {
        data = await api(`/api/thermostats/${t.id}/schedules`);
    } catch (e) {
        setMsg($('schedMsg'), e.message, false);
        return;
    }
    $('tzNote').textContent = `times are the Pi's (${data.timezone})`;
    const rows = $('schedRows');
    rows.replaceChildren();
    if (!data.schedules.length) {
        rows.append(el('tr', {}, el('td', { colspan: 6, class: 'empty' }, 'No schedules yet.')));
    }
    data.schedules.forEach((sc) => {
        const z = t.zones.find((x) => x.id === sc.zone);
        const when = sc.date ? new Date(sc.date + 'T00:00').toLocaleDateString([], { weekday: 'short', month: 'short', day: 'numeric' }) : fmtDays(sc.days);
        const toggle = el('input', { type: 'checkbox', onchange: (e) => saveSchedule({ ...sc, enabled: e.target.checked }, sc.id) });
        toggle.checked = sc.enabled;
        const tr = el('tr', {},
            cell(sc.name + (t.zones.length > 1 && z ? ` (${z.name})` : '')),
            cell(`${when} at ${sc.at}`),
            cell(describeChange(sc)),
            cell(fmtWhen(sc.last_run)),
            el('td', {}, toggle),
            el('td', {},
                el('button', { class: 'btn secondary small', onclick: () => openForm(sc) }, 'Edit'), ' ',
                el('button', { class: 'btn danger small', onclick: () => deleteSchedule(sc) }, 'Delete')));
        rows.append(tr);
    });
}

function openForm(sc) {
    const t = current();
    editingId = sc ? sc.id : null;
    $('sfZone').replaceChildren(...t.zones.map((z) => option(z.id, z.name)));
    $('sfZoneWrap').classList.toggle('hidden', t.zones.length < 2);
    const modes = t.zones[0].modes;
    $('sfMode').replaceChildren(option('', 'Leave as is'), ...modes.map((m) => option(m, MODE_LABEL[m] || m)));
    $('sfDays').replaceChildren(...DAYS.map((d, i) => {
        const cb = el('input', { type: 'checkbox', value: i });
        cb.checked = sc ? !!(sc.days & (1 << i)) : i >= 1 && i <= 5;
        return el('label', {}, cb, d);
    }));
    $('sfName').value = sc ? sc.name : '';
    $('sfRepeat').value = sc && sc.date ? 'once' : 'weekly';
    $('sfDate').value = sc ? sc.date : '';
    $('sfAt').value = sc ? sc.at : '';
    $('sfZone').value = sc ? sc.zone : t.zones[0].id;
    $('sfMode').value = sc ? sc.mode : '';
    $('sfHeat').value = sc && sc.heat != null ? sc.heat : '';
    $('sfCool').value = sc && sc.cool != null ? sc.cool : '';
    $('sfHold').value = sc ? sc.hold : 'hold';
    $('sfHeat').step = $('sfCool').step = t.unit === 'C' ? '0.5' : '1';
    repeatChanged();
    $('schedForm').classList.remove('hidden');
    $('sfName').focus();
}

function repeatChanged() {
    const once = $('sfRepeat').value === 'once';
    $('sfDateWrap').classList.toggle('hidden', !once);
    $('sfDays').classList.toggle('hidden', once);
}

async function saveSchedule(sc, id) {
    const t = current();
    try {
        if (id) await api(`/api/schedules/${id}`, { method: 'PUT', body: JSON.stringify(sc) });
        else await api(`/api/thermostats/${t.id}/schedules`, { method: 'POST', body: JSON.stringify(sc) });
        setMsg($('schedMsg'), 'Saved.', true);
        $('schedForm').classList.add('hidden');
    } catch (e) {
        setMsg($('schedMsg'), e.message, false);
    }
    loadSchedules();
    loadCommands();
}

async function deleteSchedule(sc) {
    if (!confirm(`Delete the schedule "${sc.name}"?`)) return;
    try {
        await api(`/api/schedules/${sc.id}`, { method: 'DELETE' });
        setMsg($('schedMsg'), 'Deleted.', true);
    } catch (e) {
        setMsg($('schedMsg'), e.message, false);
    }
    loadSchedules();
    loadCommands();
}

function submitForm(ev) {
    ev.preventDefault();
    const once = $('sfRepeat').value === 'once';
    const num = (v) => (v === '' ? null : Number(v));
    let days = 0;
    $('sfDays').querySelectorAll('input:checked').forEach((cb) => { days |= 1 << Number(cb.value); });
    saveSchedule({
        name: $('sfName').value,
        zone: $('sfZone').value,
        days: once ? 0 : days,
        date: once ? $('sfDate').value : '',
        at: $('sfAt').value,
        mode: $('sfMode').value,
        heat: num($('sfHeat').value),
        cool: num($('sfCool').value),
        hold: $('sfHold').value,
        enabled: true,
    }, editingId);
}

// ---- Change log ----

async function loadCommands() {
    const t = current();
    if (!t || !hasDB) return;
    let data;
    try {
        data = await api(`/api/thermostats/${t.id}/commands`);
    } catch (e) {
        return;
    }
    const rows = $('commandRows');
    rows.replaceChildren();
    if (!data.commands.length) rows.append(el('tr', {}, el('td', { colspan: 4, class: 'empty' }, 'No changes yet.')));
    data.commands.forEach((c) => {
        const result = el('td', { class: c.ok ? 'result-ok' : 'result-bad' }, c.ok ? 'OK' : 'Failed');
        if (c.error) result.append(el('div', { class: 'sub' }, c.error));
        rows.append(el('tr', {}, cell(fmtWhen(c.at)), cell(c.user), cell(c.detail || c.action), result));
    });
}

// ---- Wiring ----

$('thSelect').addEventListener('change', selectionChanged);
$('newSchedBtn').addEventListener('click', () => openForm(null));
$('sfCancel').addEventListener('click', () => $('schedForm').classList.add('hidden'));
$('sfRepeat').addEventListener('change', repeatChanged);
$('schedForm').addEventListener('submit', submitForm);
$('rangeTabs').querySelectorAll('button').forEach((b) => b.addEventListener('click', () => {
    $('rangeTabs').querySelectorAll('button').forEach((x) => x.classList.toggle('active', x === b));
    hours = Number(b.dataset.hours);
    loadHistory();
}));
$('periodTabs').querySelectorAll('button').forEach((b) => b.addEventListener('click', () => {
    $('periodTabs').querySelectorAll('button').forEach((x) => x.classList.toggle('active', x === b));
    period = b.dataset.period;
    loadCloudHistory();
}));
window.addEventListener('resize', () => { clearTimeout(window._rs); window._rs = setTimeout(loadHistory, 300); });

refresh();
setInterval(refresh, 30000);
setInterval(loadHistory, 5 * 60000);
setInterval(loadCloudHistory, 15 * 60000);
