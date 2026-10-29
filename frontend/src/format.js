export const EMPTY = { period: 'today', from: '', to: '', project: '', model: '', session: '', billing_mode: '', label: '' };
export const PERIODS = { today: 'Today', week: 'Last 7 days', month: 'This month', all: 'All time' };
export const BILLING_MODES = { payg: 'Pay as you go', subscription: 'Subscription', unknown: 'Unknown' };
// How the Muse hook decided a session's billing mode.
export const BILLING_SOURCES = { env_key: 'META_API_KEY was set', stdin_key: 'API key passed on stdin', stored_key: 'API key stored with muse auth',
  account_login: 'Meta account login with an active subscription', undetermined: 'could not be determined', backfill: 'set with meta-cost billing backfill' };

export const number = value => Number(value || 0).toLocaleString();
export const compact = value => Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 }).format(Number(value || 0));
export const equivalent = t => Number(t.payg_usd) + Number(t.subscription_equivalent_usd) + Number(t.unknown_mode_equivalent_usd);
export const tokens = t => t.input + t.output;
export const usd = value => '$' + Number(value || 0).toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: Number(value) && Math.abs(value) < 1 ? 4 : 2 });
/** "≈" marks rates assumed before their verified date; "+" marks usage that could not be priced. */
export function cost(t, key) {
  // No usage costs nothing; only usage that exists but has no rate is unknown.
  if (t.events && t.cost_unknown_events === t.events) return 'Unknown';
  const value = key ? Number(t[key]) : equivalent(t);
  return (t.cost_assumed_events ? '≈' : '') + usd(value) + (t.cost_unknown_events ? ' +' : '');
}
export const coverage = t => tokens(t) ? t.priced_tokens / tokens(t) * 100 : 100;
export const COST_NOTE = '“≈” means some usage was priced with a public rate applied before its verified date. “+” means some usage has no rate and is excluded.';
// UUIDv7 session IDs share a time-based prefix, so keep the random tail too.
export const shortId = id => id && id.length > 14 ? id.slice(0, 8) + '…' + id.slice(-4) : id;
export function dateTime(value) {
  if (!value) return '—';
  const date = new Date(value);
  const sameYear = date.getFullYear() === new Date().getFullYear();
  return date.toLocaleString(undefined, { month: 'short', day: 'numeric', ...(sameYear ? {} : { year: 'numeric' }), hour: 'numeric', minute: '2-digit' });
}
export function duration(start, end) {
  if (!start || !end) return '—';
  const minutes = Math.round((new Date(end) - new Date(start)) / 60000);
  if (minutes < 1) return 'under 1 min';
  if (minutes < 60) return `${minutes} min`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ${minutes % 60}m`;
  return `${Math.floor(hours / 24)}d ${hours % 24}h`;
}
export const percent = (part, whole) => whole ? part / whole * 100 : 0;

export function filterQuery(filters) {
  // Explicit period=all for a custom range; omit empty fields from API requests.
  return new URLSearchParams(Object.entries(filters).filter(([, value]) => value !== '' && value != null)).toString();
}
/** Detail pages are scoped by their URL; only the date range carries over. */
export const periodOnly = ({ period, from, to }) => ({ period, from, to });
export function periodLabel(filters) {
  if (filters.from || filters.to) return `${filters.from || 'Start'} – ${filters.to || 'today'}`;
  return PERIODS[filters.period];
}
export function validateFilters(value) {
  const result = { ...EMPTY };
  for (const key of Object.keys(EMPTY)) if (typeof value?.[key] === 'string') result[key] = value[key];
  if (!PERIODS[result.period]) result.period = 'today';
  if (!['', ...Object.keys(BILLING_MODES)].includes(result.billing_mode)) result.billing_mode = '';
  return result;
}
