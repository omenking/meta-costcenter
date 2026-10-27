import { useEffect, useId, useLayoutEffect, useRef, useState } from 'react';
import { createPortal } from 'react-dom';
import { Link, useNavigate } from 'react-router';
import { BILLING_MODES, EMPTY, compact, cost, coverage, dateTime, duration, equivalent, number, percent, periodLabel, shortId, tokens } from './format.js';

export function HeadingHelp({ children, text, id, icon = false, label, onClick, disabled, buttonId }) {
  const tooltipId = useId();
  const trigger = useRef(null), tooltip = useRef(null);
  const [open, setOpen] = useState(false);
  const [position, setPosition] = useState({ left: 12, top: 12 });
  useLayoutEffect(() => {
    if (!open) return;
    function place() {
      if (!trigger.current || !tooltip.current) return;
      const rect = trigger.current.getBoundingClientRect();
      const { width, height } = tooltip.current.getBoundingClientRect();
      if (rect.bottom < 0 || rect.top > innerHeight) return setOpen(false);
      setPosition({ left: Math.max(12, Math.min(rect.left, innerWidth - width - 12)), top: Math.max(12, rect.bottom + height + 8 <= innerHeight - 12 ? rect.bottom + 8 : rect.top - height - 8) });
    }
    function dismiss(event) {
      if (event.key === 'Escape' || (event.type === 'pointerdown' && !trigger.current?.contains(event.target) && !tooltip.current?.contains(event.target))) setOpen(false);
    }
    place();
    window.addEventListener('resize', place);
    document.addEventListener('scroll', place, true);
    document.addEventListener('keydown', dismiss);
    document.addEventListener('pointerdown', dismiss);
    return () => {
      window.removeEventListener('resize', place);
      document.removeEventListener('scroll', place, true);
      document.removeEventListener('keydown', dismiss);
      document.removeEventListener('pointerdown', dismiss);
    };
  }, [open, text]);
  return <>
    <button ref={trigger} type="button" id={buttonId} disabled={disabled} className={icon ? "icon-button" : "heading-help"} aria-label={label} aria-describedby={open ? tooltipId : undefined}
      onPointerEnter={() => setOpen(true)} onPointerLeave={() => { if (document.activeElement !== trigger.current) setOpen(false); }}
      onFocus={() => setOpen(true)} onBlur={() => setOpen(false)} onClick={() => { setOpen(true); onClick?.(); }}>
      <span id={id}>{children}</span>{!icon && <span className="help-icon" aria-hidden="true">?</span>}
    </button>
    {open && createPortal(<div ref={tooltip} id={tooltipId} role="tooltip" className="heading-tooltip" style={position}>{text}</div>, document.body)}
  </>;
}

/**
 * Opens a <dialog> as a modal while mounted: locks page scroll, and restores focus on close.
 * Returns props that close it on Escape. A click outside the dialog does not close it, so that
 * a stray click cannot discard what has been entered.
 */
export function useModalDialog(onClose) {
  const dialog = useRef(null);
  useEffect(() => {
    const previousFocus = document.activeElement;
    const element = dialog.current;
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    element.showModal();
    return () => { element.close(); document.body.style.overflow = previousOverflow; previousFocus?.focus(); };
  }, []);
  return {
    ref: dialog,
    onCancel: event => { event.preventDefault(); onClose(); },
  };
}

export function Filters({ filters, setFilters, options, onClose }) {
  const modal = useModalDialog(onClose);
  const change = event => {
    const { name, value } = event.target;
    setFilters(previous => ({ ...previous, [name]: value }));
  };
  const textFilter = (name, label, placeholder) => <label>{label}<input name={name} list={name + '-options'} value={filters[name]} onChange={change} placeholder={placeholder} autoComplete="off" />
    <datalist id={name + '-options'}>{(options?.[name] || []).map(value => <option key={value} value={value} />)}</datalist></label>;
  return <dialog {...modal} id="filter-sidebar" className="filter-sidebar" aria-labelledby="filter-title">
    <div className="filter-sidebar-heading"><h2 id="filter-title">Filters</h2><button type="button" aria-label="Close filters" onClick={onClose}>×</button></div>
    <form id="filters" className="filters" onSubmit={event => event.preventDefault()}>
      <label>Period<select name="period" value={filters.period} onChange={event => setFilters(previous => ({ ...previous, period: event.target.value, from: '', to: '' }))}>
        <option value="today">Today</option><option value="week">Last 7 days</option><option value="month">This month</option><option value="all">All time</option>
      </select></label>
      <label>Project<select name="project" value={filters.project} onChange={change}>
        <option value="">All projects</option>
        {filters.project && !(options.project || []).includes(filters.project) && <option value={filters.project}>{filters.project}</option>}
        {(options.project || []).map(project => <option key={project} value={project}>{project}</option>)}
      </select></label>
      <label>Model<select name="model" value={filters.model} onChange={change}>
        <option value="">All models</option>
        {filters.model && !(options.model || []).includes(filters.model) && <option value={filters.model}>{filters.model}</option>}
        {(options.model || []).map(model => <option key={model} value={model}>{model}</option>)}
      </select></label>
      <label>Billing mode<select name="billing_mode" value={filters.billing_mode} onChange={change}><option value="">All billing modes</option><option value="payg">Pay as you go</option><option value="subscription">Subscription</option><option value="unknown">Unknown</option></select></label>
      <div className="filter-dates"><label>From<input name="from" type="date" value={filters.from} onChange={change} /></label><label>To<input name="to" type="date" value={filters.to} onChange={change} /></label></div>
      {textFilter('session', 'Session', 'All sessions (includes subagents)')}
      <button type="button" id="clear" onClick={() => setFilters({ ...EMPTY })}>Reset filters</button>
    </form>
  </dialog>;
}

const CHIP_LABELS = { project: 'Project', model: 'Model', session: 'Session', billing_mode: 'Billing' };
/** Always shows the date scope; other active filters are removable chips. */
export function FilterChips({ filters, setFilters, openFilters, scoped }) {
  const active = Object.keys(CHIP_LABELS).filter(key => filters[key]);
  const remove = keys => setFilters(previous => ({ ...previous, ...Object.fromEntries(keys.map(key => [key, EMPTY[key]])) }));
  const custom = filters.from || filters.to;
  return <div className="filter-chips" aria-label="Active filters">
    <button type="button" className="chip chip-period" onClick={openFilters} title="Change period">{periodLabel(filters)}</button>
    {custom && <button type="button" className="chip" aria-label="Remove date range" onClick={() => remove(['from', 'to'])}>Custom range <span aria-hidden="true">×</span></button>}
    {!scoped && active.map(key => <button type="button" key={key} className="chip" aria-label={`Remove ${CHIP_LABELS[key].toLowerCase()} filter`} onClick={() => remove([key])}>
      {CHIP_LABELS[key]}: {key === 'billing_mode' ? BILLING_MODES[filters[key]] : key === 'session' ? shortId(filters[key]) : filters[key]} <span aria-hidden="true">×</span></button>)}
    {!scoped && active.length > 1 && <button type="button" className="chip-clear" onClick={() => remove(active)}>Clear all</button>}
    {scoped && active.length > 0 && <span className="chip-note">Project, model, session and billing filters don’t apply to this page.</span>}
  </div>;
}

const UNIT_NAMES = { day: 'day', hour: 'hour', '15m': '15 minutes' };
const RESOLUTIONS = [['15m', '15 min'], ['hour', 'Hour'], ['day', 'Day']];
/** Every how many bars an axis label goes; intraday axes are labelled on round hours. */
function tickStride(unit, count, aligned, width) {
  if (!aligned || unit === 'day') return count <= 7 ? 1 : Math.ceil(count / 6);
  const perHour = unit === '15m' ? 4 : 1, labels = Math.max(3, Math.min(8, Math.floor((width - 80) / 60)));
  return perHour * ([1, 2, 3, 4, 6, 12, 24, 48].find(hours => count / (hours * perHour) <= labels) || 96);
}
function chartLabel(key, unit, index, points, aligned) {
  if (unit === 'day') return key.slice(5);
  if (aligned) return points.length > (unit === '15m' ? 96 : 24) && key.endsWith('T00:00') ? key.slice(5, 10) : key.slice(11);
  const sameDay = points.every(point => point.bucket.slice(0, 10) === points[0].bucket.slice(0, 10));
  return sameDay || (index > 0 && points[index - 1].bucket.slice(0, 10) === key.slice(0, 10)) ? key.slice(11) : key.slice(5, 10) + ' ' + key.slice(11);
}
/** Stacked input/output token bars, or estimated cost bars with unpriced periods marked. An aligned
 * timeline has every bucket of its range, so its labels fall on round hours and now can be marked. */
export function UsageChart({ timeline, unit = 'day', metric = 'tokens', current = '', aligned = false }) {
  const container = useRef(null);
  const [width, setWidth] = useState(600);
  useLayoutEffect(() => {
    const observer = new ResizeObserver(entries => setWidth(Math.max(300, entries[0].contentRect.width)));
    observer.observe(container.current);
    return () => observer.disconnect();
  }, []);
  const isCost = metric === 'cost';
  const max = Math.max(...timeline.map(point => isCost ? equivalent(point) : point.input + point.output), isCost ? 0.01 : 1);
  const axis = value => isCost ? '$' + (max < 10 ? value.toFixed(2) : compact(value)) : compact(value);
  const step = (width - 80) / Math.max(timeline.length, 1), bar = Math.max(1, Math.min(24, step * .55));
  const stride = tickStride(unit, timeline.length, aligned, width), nowIndex = timeline.findIndex(point => point.bucket === current);
  return <div ref={container} id="chart" className="chart" role="img" aria-label={`${isCost ? 'Estimated cost' : 'Token usage'} by ${UNIT_NAMES[unit]}`}>
    {!timeline.length ? <span>No observed usage in this date range.</span> : <svg viewBox={`0 0 ${width} 205`} role="presentation">
      {[0, 1, 2, 3].map(i => <g key={i}><line x1="68" y1={16 + i * 49} x2={width - 8} y2={16 + i * 49} stroke="#b8c1cd" strokeDasharray="3 4" /><text x="60" y={22 + i * 49} textAnchor="end">{axis(max * (1 - i / 3))}</text></g>)}
      {nowIndex >= 0 && <g className="now-marker"><line x1={70 + step * (nowIndex + .5)} y1="10" x2={70 + step * (nowIndex + .5)} y2="163" /><title>Now</title></g>}
      {timeline.map((point, i) => {
        const x = 70 + step * (i + .5) - bar / 2, label = point.bucket.replace('T', ' ') + (unit === '15m' ? ' (15 min)' : '');
        const tick = (i % stride === 0 || (!aligned && i === timeline.length - 1)) && <text x={x + bar / 2} y="190" textAnchor="middle">{chartLabel(point.bucket, unit, i, timeline, aligned)}</text>;
        if (isCost) {
          const height = equivalent(point) / max * 147, unpriced = point.cost_unknown_events > 0;
          return <g key={point.bucket} className="cost-bar"><rect x={x} y={163 - height} width={bar} height={height} fill="#005c45" rx="2">
            <title>{label}: {cost(point)}{unpriced ? ` · ${number(point.cost_unknown_events)} completions unpriced` : ''}</title></rect>
            {unpriced && <circle cx={x + bar / 2} cy={163 - height - 7} r="3.5" fill="#bb6c00"><title>{label}: {number(point.cost_unknown_events)} completions have no rate</title></circle>}{tick}</g>;
        }
        const input = point.input / max * 147, output = point.output / max * 147;
        return <g key={point.bucket}><rect x={x} y={163 - input} width={bar} height={input} fill="#005c45" rx="2"><title>{label}: {number(point.input)} input, {number(point.output)} output</title></rect><rect x={x} y={163 - input - output} width={bar} height={output} fill="#bb6c00" rx="1" />{tick}</g>;
      })}
    </svg>}
  </div>;
}
/** The usage chart. Given a chart from the API it draws that, with a choice of the resolutions the
 * date range allows; otherwise it draws a plain timeline in its given unit. */
export function ChartPanel({ timeline, unit = 'day', chart, setResolution }) {
  const [metric, setMetric] = useState(() => { try { return localStorage.getItem('muse-cost.chart') || 'cost'; } catch { return 'cost'; } });
  function choose(value) { setMetric(value); try { localStorage.setItem('muse-cost.chart', value); } catch { /* Storage may be disabled. */ } }
  if (chart) { timeline = chart.points; unit = chart.unit; }
  const isCost = metric === 'cost', period = { hour: 'hour', '15m': '15-minute block' }[unit] || 'calendar day';
  return <article className="panel"><div className="panel-heading"><div><h2>{isCost ? 'Cost over time' : 'Usage over time'}</h2>
    <p>{isCost ? `Estimated API-equivalent USD by local ${period}` : `Measured tokens by local ${period}`}</p></div>
    <div className="chart-controls">
      {chart && setResolution && chart.units.length > 1 && <div className="tabs" role="group" aria-label="Chart resolution">{RESOLUTIONS.filter(([key]) => chart.units.includes(key)).map(([key, label]) =>
        <button key={key} type="button" data-resolution={key} className={unit === key ? 'selected' : ''} aria-pressed={unit === key} onClick={() => setResolution(key)}>{label}</button>)}</div>}
      <div className="tabs" role="group" aria-label="Chart shows">{[['cost', 'Cost'], ['tokens', 'Tokens']].map(([key, label]) => <button key={key} type="button" data-metric={key} className={metric === key ? 'selected' : ''} aria-pressed={metric === key} onClick={() => choose(key)}>{label}</button>)}</div>
      <div className="legend">{isCost ? <><span className="dot input-dot" />Estimated<span className="dot output-dot" />Has unpriced usage</> : <><span className="dot input-dot" />Input<span className="dot output-dot" />Output</>}</div>
    </div></div><UsageChart timeline={timeline} unit={unit} metric={metric} current={chart?.current} aligned={Boolean(chart)} /></article>;
}

export function Metric({ label, icon, id, value, description, descriptionId, accent, to }) {
  const body = <><div className="metric-label">{label}<span aria-hidden="true">{icon}</span></div><strong id={id}>{value}</strong><div id={descriptionId}>{description}</div></>;
  return to ? <Link className={'metric metric-link' + (accent ? ' accent' : '')} to={to}>{body}</Link> : <article className={'metric' + (accent ? ' accent' : '')}>{body}</article>;
}
export function CostMetric({ totals }) {
  const covered = coverage(totals);
  return <Metric label="Estimated cost" icon="$" id="cost" value={cost(totals)} descriptionId="cost-label"
    description={!totals.events ? 'No usage in this period' : covered >= 99.95 ? 'All usage priced · API-equivalent USD'
      : <>{covered.toFixed(1)}% of tokens priced · <Link className="inline-link" to="/pricing">set missing rates</Link></>} />;
}
export function TotalsMetrics({ totals, extra }) {
  return <section className="metrics" aria-label="Usage totals">
    <CostMetric totals={totals} />
    <Metric label="Input tokens" icon="↗" id="input" value={number(totals.input)} descriptionId="cached-label" description={`${number(totals.cached)} cached${totals.cache_unknown_events ? ` · ${totals.cache_unknown_events} cache counts unknown` : ''}`} />
    <Metric label="Output tokens" icon="↙" id="output" value={number(totals.output)} descriptionId="events-label" description={`${number(totals.events)} observed model completions`} />
    {extra}
  </section>;
}

export function EmptyRow({ children, columns = 5 }) { return <tr className="empty"><td colSpan={columns}>{children}</td></tr>; }
export function Share({ value }) {
  return <><span className="share"><span className="share-fill" style={{ display: 'block', width: value + '%' }} /></span>{value.toFixed(1)}%</>;
}
export function NotFound({ kind, message }) {
  return <section className="panel empty-panel"><h2>{kind} not found</h2><p>{message}</p><Link className="button" to={kind === 'Session' ? '/sessions' : '/projects'}>Back to {kind.toLowerCase()}s</Link></section>;
}

/** Sortable table state: clicking the active column flips direction. */
export function useSort(initial) {
  const [sort, setSort] = useState(initial);
  const header = (key, label, help) => {
    const active = sort.key === key;
    return <th scope="col" aria-sort={active ? (sort.desc ? 'descending' : 'ascending') : undefined}>
      <button type="button" className="sort-button" title={help} onClick={() => setSort({ key, desc: active ? !sort.desc : true })}>{label}<span aria-hidden="true">{active ? (sort.desc ? ' ↓' : ' ↑') : ''}</span></button></th>;
  };
  const apply = (rows, value) => [...rows].sort((a, b) => { const x = value(a, sort.key), y = value(b, sort.key); return (x < y ? -1 : x > y ? 1 : 0) * (sort.desc ? -1 : 1); });
  return { header, apply };
}

const SESSION_SORT = { last_activity: s => s.last_activity, tokens, cost: equivalent, subagents: s => s.subagents };
export function SessionsTable({ sessions, id = 'session-rows', empty = 'No sessions match the current filters. Try a longer period or clear filters.', showProject = true, limit }) {
  const navigate = useNavigate();
  const { header, apply } = useSort({ key: 'last_activity', desc: true });
  const rows = apply(sessions, (row, key) => SESSION_SORT[key](row)).slice(0, limit);
  return <div className="table-wrap"><table className="sessions-table"><thead><tr>
    <th scope="col">Session</th>{header('last_activity', 'Last activity')}<th scope="col">Models</th>
    {header('tokens', 'Input / output')}{header('cost', 'Est. cost', 'API-equivalent estimate in USD; not a billed charge. ≈ assumed rate, + partly unpriced')}{header('subagents', 'Subagents')}
  </tr></thead><tbody id={id}>
    {!rows.length && <EmptyRow columns={6}>{empty}</EmptyRow>}
    {rows.map(row => <tr key={row.id} className="clickable" onClick={event => { if (!event.target.closest('a')) navigate('/sessions/' + encodeURIComponent(row.id)); }}>
      <td><Link className="row-link" to={'/sessions/' + encodeURIComponent(row.id)} title={row.id}>{showProject ? row.project : shortId(row.id)}</Link>
        <span className="secondary-text">{showProject ? shortId(row.id) : row.billing_modes.map(mode => BILLING_MODES[mode] || mode).join(', ')}</span></td>
      <td title={`Started ${dateTime(row.started)}`}>{dateTime(row.last_activity)}<span className="secondary-text">ran {duration(row.started, row.last_activity)}</span></td>
      <td title={row.models.join(', ')}>{row.models[0]}{row.models.length > 1 && <span className="secondary-text">+{row.models.length - 1} more</span>}</td><td>{number(row.input)} / {number(row.output)}</td><td>{cost(row)}</td><td>{row.subagents || '—'}</td>
    </tr>)}
  </tbody></table></div>;
}

export function ModelsTable({ models, total }) {
  return <div className="table-wrap"><table><thead><tr><th scope="col">Model</th><th scope="col">Input tokens</th><th scope="col">Output tokens</th><th scope="col">API-equivalent cost</th><th scope="col">Token share</th></tr></thead><tbody>
    {!models.length && <EmptyRow>No usage in this period.</EmptyRow>}
    {models.map(row => <tr key={row.label}><td>{row.label}</td><td>{number(row.input)}</td><td>{number(row.output)}</td><td>{cost(row)}</td><td><Share value={percent(tokens(row), total)} /></td></tr>)}
  </tbody></table></div>;
}
