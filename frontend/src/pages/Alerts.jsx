import { useEffect, useState } from 'react';
import { useLocation, useNavigate } from 'react-router';
import { useModalDialog } from '../components.jsx';
import { jsonRequest, send, useResource } from '../api.js';
import { number, usd } from '../format.js';

const PERIODS = { day: 'Day', week: 'Week (from Monday)', month: 'Month' };
const BLANK = { period: 'day', project: '', billing_mode: '', limit_usd: '', warn_levels: [80], revoke_keys: false, key_match: '*' };
const MAX_WARNINGS = 4;
// Suggested values for the optional warning levels when they are first switched on.
const SUGGESTED = [80, 50, 90, 95];

/** Four warning slots: the first is always on; saved levels fill slots in ascending order. */
function warningSlots(levels) {
  const unused = SUGGESTED.filter(value => !levels.includes(value));
  return Array.from({ length: MAX_WARNINGS }, (_, i) => i < levels.length ? { on: true, value: levels[i] } : { on: false, value: unused.shift() ?? '' });
}

export function windowText(alert) {
  if (alert.period === 'day') return 'today';
  const start = new Date(alert.window_start + 'T00:00').toLocaleDateString(undefined, { weekday: alert.period === 'week' ? 'short' : undefined, month: 'short', day: 'numeric' });
  return `since ${start}`;
}

export const alertText = alert => `${alert.label}: ${usd(alert.spent_usd)} of ${usd(alert.limit_usd)} ${windowText(alert)} (${alert.percent}%)`
  + (alert.level === 'warning' && alert.warning_at ? ` · passed your ${alert.warning_at}% warning` : '')
  + (alert.level === 'exceeded' ? keyguardText(alert.keyguard) : '');

/** Colour step 0–3 for a warning level among an alert's levels, so later warnings run from amber towards red. */
function warnStep(levels, value) {
  const sorted = [...levels].sort((a, b) => a - b);
  return sorted.length < 2 ? 1 : Math.round(sorted.indexOf(value) * 3 / (sorted.length - 1));
}

/** The class for an alert card: its level and, while warning, the step of the warning it passed. */
export const alertClass = alert => 'level-' + alert.level + (alert.level === 'warning' && alert.warning_at ? ' warn-step-' + warnStep(alert.warn_levels || [], alert.warning_at) : '');

/** Spend against the limit, with a tick at each warning level in that level's colour. */
export function AlertMeter({ alert, label }) {
  const levels = alert.warn_levels || [];
  return <div className="alert-meter" role="meter" aria-valuemin={0} aria-valuemax={100} aria-valuenow={Math.min(alert.percent, 100)} aria-label={label}>
    <span style={{ width: Math.min(alert.percent, 100) + '%' }} />{levels.map(value => <i key={value} className={'warn-step-' + warnStep(levels, value)} style={{ left: value + '%' }} title={`Warns at ${value}%`} />)}</div>;
}

/** "20%, 40%, …" with a dot in each level's colour, as a legend for the meter's ticks. */
export const WarnLevels = ({ levels }) => levels.map((value, i) => <span key={value}>{i ? ', ' : ''}<span className={'warn-level warn-step-' + warnStep(levels, value)}>{value}%</span></span>);

const time = stamp => new Date(stamp).toLocaleTimeString(undefined, { hour: 'numeric', minute: '2-digit' });
const minutes = ms => { const m = Math.max(0, Math.round(ms / 60000)); return m >= 60 ? `${Math.floor(m / 60)}h ${m % 60}m` : `${m}m`; };
const keyList = keys => keys.length > 3 ? `${keys.slice(0, 3).join(', ')} and ${keys.length - 3} more` : keys.join(', ');

/** What Meta Keyguard has done about an exceeded alert, for the alert bar and notifications. */
function keyguardText(info) {
  if (!info) return '';
  if (info.revoked) return ` · Keyguard revoked ${info.revoked.keys.length} key(s) at ${time(info.revoked.at)}`;
  if (!info.enforced) return ' · keys NOT revoked: Keyguard is not enforcing this alert';
  return ' · Keyguard is revoking matching keys';
}

/** The Keyguard line under an alert that revokes keys. */
function KeyguardStatus({ alert }) {
  const info = alert.keyguard;
  if (!info) return null;
  let text, tone;
  if (info.revoked) {
    text = `Keyguard revoked ${keyList(info.revoked.keys)} at ${time(info.revoked.at)}.`;
    tone = 'done';
  } else if (!info.enforced) {
    text = `Not enforced: ${info.reason}`;
    tone = 'bad';
  } else {
    const covered = info.matching || [];
    text = covered.length ? `Covers ${covered.length} key(s) now: ${keyList(covered)}.` : 'Matches no current keys.';
    tone = covered.length ? '' : 'warn';
  }
  return <p className={'keyguard-status ' + tone}>{text}{info.warning && <span className="keyguard-warning"> {info.warning}</span>}</p>;
}

/** Live preview of the keys a key match covers, shown under the field in the alert dialog. */
function KeyMatchPreview({ match }) {
  const [state, setState] = useState(null);
  useEffect(() => {
    if (!match.trim()) { setState(null); return; }
    let active = true;
    const timer = setTimeout(() => {
      jsonRequest('/api/keyguard?match=' + encodeURIComponent(match))
        .then(data => active && setState({ data }))
        .catch(failure => active && setState({ error: failure.message }));
    }, 300);
    return () => { active = false; clearTimeout(timer); };
  }, [match]);
  if (!state) return null;
  if (state.error) return <p className="field-help keyguard-preview bad" id="key-match-preview">{state.error}</p>;
  const { data } = state;
  if (!data.available) return <p className="field-help keyguard-preview warn" id="key-match-preview">Cannot preview: {data.error}</p>;
  const keys = data.preview || [];
  return <p className={'field-help keyguard-preview' + (keys.length ? '' : ' warn')} id="key-match-preview">
    {keys.length ? `Matches ${keys.length} key(s) now: ${keyList(keys)}` : 'Matches no current keys'}
    {!data.enforced && <><br />{data.reason}</>}</p>;
}

/** Whether Meta Keyguard is installed and enforcing alerts, and the keys it manages. */
function KeyguardPanel({ revoking }) {
  const { data, error } = useResource('/api/keyguard');
  if (!data && !error) return null;
  if (error) return <section className="panel" id="keyguard"><h2>Meta Keyguard</h2><p className="notice danger">{error}</p></section>;
  if (!data.installed && !revoking) return null;
  const now = Date.now();
  return <section className="panel" id="keyguard"><div className="panel-heading"><div><h2>Meta Keyguard</h2>
    <p>Meta Keyguard deletes Meta API keys for alerts set to revoke them. Cost Center only reports what it does.</p></div></div>
    {!data.available ? <p className="notice danger" id="keyguard-state">{data.error}</p>
      : !data.enforced ? <p className="notice danger" id="keyguard-state">{revoking ? `${revoking} alert(s) are set to revoke keys but are not enforced. ` : ''}{data.reason}</p>
      : <p className="notice ok" id="keyguard-state">Enforcing alerts that revoke keys{data.cost_center_check?.last_check?.at ? `; last checked at ${time(data.cost_center_check.last_check.at)}` : ''}.{data.warning && <> {data.warning}</>}</p>}
    {data.available && data.last_failure && <p className="notice danger">Keyguard's last operation failed ({data.last_failure.op}, {data.last_failure.code}): {data.last_failure.message}</p>}
    {data.available && <div className="table-wrap"><table id="keyguard-keys"><thead><tr><th>Key</th><th>State</th><th>Expires</th></tr></thead><tbody>
      {data.keys.length ? data.keys.map(k => <tr key={k.meta_name}><td>{k.label}</td><td>{k.overdue ? 'overdue for deletion' : k.state}</td>
        <td>{k.expires_at ? (k.overdue ? 'past due' : `in ${minutes(Date.parse(k.expires_at) - now)}`) : '–'}</td></tr>)
        : <tr><td colSpan="3" className="muted">Keyguard manages no live keys.</td></tr>}
    </tbody></table>
    <p className="table-footer">{(data.targets || []).filter(t => !t.managed).length} other key(s) on dev.meta.ai{data.targets_as_of ? `, as of ${time(data.targets_as_of)}` : ''}.</p></div>}
  </section>;
}

/** Sample alerts for previewing the alert bar; nothing is saved. */
export const SAMPLE_ALERTS = {
  exceeded: { id: 'preview', preview: true, level: 'exceeded', period: 'day', label: 'Daily limit for all projects', spent_usd: 6.1, limit_usd: 5, percent: 122 },
  warning: { id: 'preview', preview: true, level: 'warning', period: 'day', label: 'Daily limit for all projects', spent_usd: 4.2, limit_usd: 5, percent: 84, warning_at: 80 },
};

/** Add or edit an alert in a modal dialog. */
function AlertDialog({ initial, projects, onSaved, onClose }) {
  const modal = useModalDialog(onClose);
  const [values, setValues] = useState({ ...BLANK, ...initial });
  const [warnings, setWarnings] = useState(() => warningSlots(initial.warn_levels || BLANK.warn_levels));
  const setWarning = (index, change) => setWarnings(previous => previous.map((slot, i) => i === index ? { ...slot, ...change } : slot));
  const [error, setError] = useState('');
  const [saving, setSaving] = useState(false);
  const change = event => setValues(previous => ({ ...previous, [event.target.name]: event.target.value }));
  async function save(event) {
    event.preventDefault();
    const body = { ...values, warn_levels: warnings.filter(slot => slot.on).map(slot => Number(slot.value)), project: values.project || null, billing_mode: values.billing_mode || null,
      key_match: values.revoke_keys ? values.key_match : null };
    setSaving(true);
    try {
      await (values.id ? send('PUT', '/api/alerts/' + values.id, body) : send('POST', '/api/alerts', body));
      onSaved();
    } catch (failure) { setError(failure.message); setSaving(false); }
  }
  const projectOptions = values.project && !projects.includes(values.project) ? [values.project, ...projects] : projects;
  return <dialog {...modal} id="alert-dialog" className="modal" aria-labelledby="alert-dialog-title">
    <div className="modal-heading"><h2 id="alert-dialog-title">{values.id ? 'Edit alert' : 'Add alert'}</h2><button type="button" aria-label="Close" onClick={onClose}>×</button></div>
    <form className="alert-form" id="alert-form" onSubmit={save}>
      <p className="modal-intro">Get warned when estimated cost passes a limit. Alerts never stop usage themselves, although Meta Keyguard can revoke keys when you allow it below.</p>
      <label>Period<select name="period" value={values.period} onChange={change}>{Object.entries(PERIODS).map(([key, label]) => <option key={key} value={key}>{label}</option>)}</select></label>
      <label>Project<select name="project" value={values.project || ''} onChange={change}><option value="">All projects</option>{projectOptions.map(project => <option key={project} value={project}>{project}</option>)}</select></label>
      <label>Usage<select name="billing_mode" value={values.billing_mode || ''} onChange={change}><option value="">All usage</option><option value="payg">Pay as you go only</option></select></label>
      <fieldset className="warning-levels"><legend>Warn me at</legend>
        <p className="field-help">Percent of the limit. Each level notifies you once per period.</p>
        {warnings.map((slot, i) => <div key={i} className={'warning-slot' + (slot.on ? '' : ' off')}>
          {i === 0 ? <span className="warning-check" aria-hidden="true">✓</span>
            : <input type="checkbox" id={`warning-on-${i}`} checked={slot.on} onChange={event => setWarning(i, { on: event.target.checked })} aria-label={`Use warning ${i + 1}`} />}
          <label htmlFor={`warning-${i}`}>Warning {i + 1}</label>
          <span className="percent-input"><input id={`warning-${i}`} name={`warning-${i}`} type="number" min="1" max="99" step="1" required={slot.on} disabled={!slot.on}
            value={slot.value} onChange={event => setWarning(i, { value: event.target.value })} /><span aria-hidden="true">%</span></span>
        </div>)}
      </fieldset>
      <label>Limit (USD)<input name="limit_usd" type="number" min="0.01" step="0.01" required value={values.limit_usd} onChange={change} placeholder="5.00" /></label>
      <fieldset className="key-revocation"><legend>Meta Keyguard</legend>
        <label className="checkbox-label"><input type="checkbox" name="revoke_keys" checked={values.revoke_keys} onChange={event => setValues(previous => ({ ...previous, revoke_keys: event.target.checked }))} />
          Revoke Meta API keys when the limit is exceeded</label>
        <p className="field-help">Meta Keyguard deletes matching keys on dev.meta.ai when its Cost Center check is turned on. Use * for every key, bootcamp-* for a prefix, or key1,key2 for specific keys.</p>
        <label>Keys to revoke<input name="key_match" type="text" required={values.revoke_keys} disabled={!values.revoke_keys} value={values.key_match || ''} onChange={change} placeholder="*" aria-describedby="key-match-preview" /></label>
        {values.revoke_keys && <KeyMatchPreview match={values.key_match || ''} />}
      </fieldset>
      {error && <p className="form-error" role="alert">{error}</p>}
      <div className="modal-actions"><button type="button" onClick={onClose}>Cancel</button><button type="submit" className="primary" disabled={saving}>{values.id ? 'Save alert' : 'Add alert'}</button></div>
    </form>
  </dialog>;
}

function NotificationControl() {
  const supported = typeof Notification !== 'undefined';
  const [permission, setPermission] = useState(supported ? Notification.permission : 'unsupported');
  if (permission === 'unsupported') return <p className="muted">This browser does not support desktop notifications.</p>;
  if (permission === 'granted') return <div className="notification-control"><p className="muted" id="notification-state">Desktop notifications are on. They appear while a Muse Cost tab is open.</p>
    <button type="button" id="test-notification" className="small-button" onClick={() => new Notification('Muse Cost: test notification', { body: alertText(SAMPLE_ALERTS.exceeded), icon: '/logo.png' })}>Send test notification</button></div>;
  if (permission === 'denied') return <p className="muted" id="notification-state">Desktop notifications are blocked for this page. Allow them in your browser’s site settings to receive alerts.</p>;
  return <button type="button" id="enable-notifications" onClick={() => Notification.requestPermission().then(setPermission)}>Enable desktop notifications</button>;
}

export default function Alerts({ options, onPreview }) {
  const location = useLocation(), navigate = useNavigate();
  const { data, error, reload } = useResource('/api/alerts');
  // null when closed; otherwise the values the dialog opens with (an id means edit).
  const [dialog, setDialog] = useState(() => location.state?.project ? { project: location.state.project } : null);
  const [actionError, setActionError] = useState('');
  const projects = options.project || [];
  // A project page's "Add alert" link opens the dialog once; clear it so refresh or back doesn't reopen it.
  useEffect(() => { if (location.state?.project) navigate(location.pathname, { replace: true, state: null }); }, [location, navigate]);
  async function act(request) {
    setActionError('');
    try { await request; reload(); } catch (failure) { setActionError(failure.message); }
  }
  const toggle = alert => act(send('PUT', '/api/alerts/' + alert.id, { ...alert, enabled: !alert.enabled }));
  const remove = alert => { if (confirm(`Delete the ${alert.label.toLowerCase()}?`)) act(send('DELETE', '/api/alerts/' + alert.id)); };
  return <>
    {(error || actionError) && <div className="notice danger" role="alert">{error || actionError}</div>}
    <section className="panel" id="alerts"><div className="panel-heading"><div><h2>Your alerts</h2><p>Get warned when estimated cost passes a limit for a day, a week, or a month, for all projects or one. Unpriced usage is not counted. Alerts never stop usage themselves, but an alert can ask Meta Keyguard to revoke API keys.</p></div>
      <button type="button" id="add-alert" className="primary" onClick={() => setDialog({})}>Add alert</button></div>
      {!data ? <p className="panel-empty">Loading alerts…</p> : !data.length ? <p className="panel-empty">No alerts yet. Add a daily limit to start.</p> :
        <ul className="alert-list" id="alert-rows">{data.map(alert => <li key={alert.id} className={'alert-item ' + alertClass(alert)}>
          <div className="alert-summary"><strong>{alert.label}</strong>
            <span className="alert-status">{alert.level === 'paused' ? 'Paused' : `${usd(alert.spent_usd)} of ${usd(alert.limit_usd)} ${windowText(alert)}`}</span></div>
          <AlertMeter alert={alert} label={`${alert.percent}% of limit`} />
          <div className="alert-details"><span>{alert.percent}% used · warns at <WarnLevels levels={alert.warn_levels} />{alert.revoke_keys ? ` · revokes keys matching ${alert.key_match}` : ''}{alert.unpriced_events ? ` · ${number(alert.unpriced_events)} unpriced completions not counted` : ''}{alert.unknown_billing_events ? ` · ${number(alert.unknown_billing_events)} completions with unknown billing mode not counted` : ''}</span>
            <span className="row-actions"><button type="button" className="small-button" onClick={() => setDialog({ id: alert.id, period: alert.period, project: alert.project || '', billing_mode: alert.billing_mode || '', limit_usd: alert.limit_usd, warn_levels: alert.warn_levels, enabled: alert.enabled, revoke_keys: alert.revoke_keys, key_match: alert.key_match || '*' })}>Edit</button>
              <button type="button" className="small-button" onClick={() => toggle(alert)}>{alert.enabled ? 'Pause' : 'Resume'}</button>
              <button type="button" className="small-button" onClick={() => remove(alert)}>Delete</button></span></div>
          {alert.unknown_billing_events > 0 && <p className="keyguard-status warn billing-unknown">This alert counts only pay-as-you-go usage, but {number(alert.unknown_billing_events)} completion(s) in this window have no recorded billing mode, so they are not counted. Run <code>meta-cost hook install</code> so that new Muse sessions record whether they use the subscription or an API key.</p>}
          <KeyguardStatus alert={alert} />
        </li>)}</ul>}
    </section>
    <KeyguardPanel revoking={(data || []).filter(alert => alert.revoke_keys && alert.enabled).length} />
    <section className="panel preview-panel" id="alert-preview"><div><h2>Notifications and preview</h2><p>See how the alert bar looks with sample numbers. Nothing is saved.</p></div>
      <div className="preview-controls"><NotificationControl />
        <div className="form-actions"><button type="button" id="preview-exceeded" onClick={() => onPreview(SAMPLE_ALERTS.exceeded)}>Preview limit exceeded</button>
          <button type="button" id="preview-warning" onClick={() => onPreview(SAMPLE_ALERTS.warning)}>Preview warning</button></div></div></section>
    {dialog && <AlertDialog initial={dialog} projects={projects} onSaved={() => { setDialog(null); reload(); }} onClose={() => setDialog(null)} />}
  </>;
}
