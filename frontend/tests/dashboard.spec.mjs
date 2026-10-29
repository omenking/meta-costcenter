// End-to-end checks of the dashboard: `meta-cost show` over a fixture of logs, driven in Chrome.
// Run with `npm run test:browser`. It builds the binary with Go unless META_COST names one.
import { test, expect } from '@playwright/test';
import { execFileSync, spawn } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

const repo = path.resolve(import.meta.dirname, '../..');

function event(id, session, input, cached, model) {
  return {
    id, stream: { id: session }, sequence: 1, recorded_at: Date.now() * 1000,
    payload: {
      event: { kind: 'model_completed', model, usage: { input_tokens: input, output_tokens: 10, cached_tokens: cached },
        response: 'SECRET_RESPONSE' },
      prompt: 'SECRET_PROMPT',
    },
  };
}

const metadata = (project) => ({ payload_type: 'runtime.session.metadata', payload: { record: { workspace_root: `/demo/${project}` } } });
const jsonl = (records) => records.map((r) => JSON.stringify(r) + '\n').join('');

let tmp, logs, server, base;

test.beforeAll(async () => {
  tmp = fs.mkdtempSync(path.join(os.tmpdir(), 'meta-cost-browser-'));
  logs = path.join(tmp, 'logs');
  ['Website', 'CLI tools', 'Research', 'Website'].forEach((project, i) => {
    const file = path.join(logs, `session-${i}`, 'session.jsonl');
    fs.mkdirSync(path.dirname(file), { recursive: true });
    const events = [0, 1, 2].map((j) => event(`event-${i}-${j}`, `session-${i}`, 10000 * (i + 1), 5000, 'muse-spark-1.3'));
    fs.writeFileSync(file, jsonl([metadata(project), ...events]));
  });
  // A subagent of session-3 with output-only usage, so input totals stay round.
  const sub = path.join(logs, 'session-3', 'subagent', 'sub-3a', 'session.jsonl');
  fs.mkdirSync(path.dirname(sub), { recursive: true });
  fs.writeFileSync(sub, jsonl([metadata('Website'), ...[0, 1].map((j) => event(`sub-event-${j}`, 'sub-3a', 0, 0, 'muse-spark-1.3'))]));

  let binary = process.env.META_COST;
  if (!binary) {
    binary = path.join(tmp, 'meta-cost');
    execFileSync('go', ['build', '-o', binary, './cmd/meta-cost'], { cwd: repo, stdio: 'inherit' });
  }
  server = spawn(binary, ['show', '--no-browser', '--port', '0', '--interval', '0.1', '--period', 'all',
    '--db', path.join(tmp, 'usage.sqlite3'), '--source', logs], {
    env: { ...process.env, XDG_CONFIG_HOME: tmp, XDG_DATA_HOME: tmp },
    stdio: ['ignore', 'pipe', 'inherit'],
  });
  base = await new Promise((resolve, reject) => {
    let output = '';
    server.stdout.on('data', (chunk) => {
      output += chunk;
      const match = output.match(/Muse Cost dashboard: (http:\/\/\S+?)\/?\n/);
      if (match) resolve(match[1]);
    });
    server.on('exit', (code) => reject(new Error(`meta-cost show exited ${code}: ${output}`)));
  });
});

test.afterAll(() => {
  server?.kill();
  if (tmp) fs.rmSync(tmp, { recursive: true, force: true });
});

test('dashboard', async ({ page }) => {
  await page.setViewportSize({ width: 1440, height: 1100 });
  const errors = [], documents = [];
  page.on('pageerror', (error) => errors.push(String(error)));
  page.on('request', (request) => { if (request.resourceType() === 'document') documents.push(request.url()); });
  const filtersButton = () => page.getByRole('button', { name: 'Filters', exact: true });
  const openFilters = async () => {
    await filtersButton().click();
    await expect(page.getByRole('dialog', { name: 'Filters' })).toBeVisible();
  };
  const closeFilters = async () => {
    await page.getByRole('button', { name: 'Close filters' }).click();
    await expect(page.getByRole('dialog')).toHaveCount(0);
  };
  const checkFilter = async (name, value) => {
    await openFilters();
    await expect(page.locator(`[name=${name}]`)).toHaveValue(value);
    await closeFilters();
  };
  const input = page.locator('#input');

  await page.goto(base + '/');
  await expect(input).toHaveText('300,000');
  await expect(page.getByRole('dialog')).toHaveCount(0);
  expect(page.url()).toBe(base + '/');
  await openFilters();
  await expect(page.locator('[name=period]')).toHaveValue('all');
  await page.keyboard.press('Escape');
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await expect(filtersButton()).toBeFocused();
  await page.getByRole('button', { name: 'Export usage' }).hover();
  await expect(page.getByRole('tooltip')).toContainText('current filters');
  await page.keyboard.press('Escape');

  await page.setViewportSize({ width: 1440, height: 420 });
  for (const [label, heading, rows] of [['Overview', 'Usage overview', '#input'], ['Sessions', 'Sessions', '#session-rows'],
    ['Projects', 'Projects', '#project-rows'], ['Breakdown', 'Usage breakdown', '#breakdown-rows']]) {
    await page.locator('.nav', { hasText: label }).click();
    await expect(page.locator('h1')).toHaveText(heading);
    await page.waitForSelector(rows);
    expect((await page.locator('.page-header').boundingBox()).height).toBeLessThanOrEqual(80);
    await page.evaluate(() => window.scrollTo({ top: 600, behavior: 'instant' }));
    expect(await page.evaluate(() => window.scrollY)).toBeGreaterThan(0);
    expect(Math.abs((await page.locator('.page-header').boundingBox()).y)).toBeLessThan(2);
    await expect(filtersButton()).toBeInViewport();
  }
  await page.locator('.nav', { hasText: 'Overview' }).click();
  await page.setViewportSize({ width: 1440, height: 1100 });
  await expect(input).toHaveText('300,000');

  await openFilters();
  const project = page.locator('[name=project]');
  await project.selectOption('Website');
  await expect(input).toHaveText('150,000');
  // Switching projects must keep every project available without clearing first.
  await expect(page.locator('[name=project] option')).toHaveCount(4);
  await project.selectOption('Research');
  await expect(input).toHaveText('90,000');
  await project.selectOption('CLI tools');
  await expect(input).toHaveText('60,000');
  await project.selectOption('');
  await expect(input).toHaveText('300,000');
  await project.selectOption('Website');
  await expect(input).toHaveText('150,000');
  await page.screenshot({ path: path.join(os.tmpdir(), 'meta-cost-filters-desktop.png'), fullPage: true });
  await closeFilters();
  expect(page.url()).toBe(base + '/');

  await page.locator('.nav', { hasText: 'Breakdown' }).click();
  await expect(page.locator('h1')).toHaveText('Usage breakdown');
  await checkFilter('project', 'Website');
  await expect(page.locator('#breakdown-rows tr')).toHaveCount(1);
  expect(documents).toHaveLength(1);
  await page.getByRole('button', { name: 'Token share' }).hover();
  await expect(page.getByRole('tooltip')).toContainText('not a share of cost');
  await page.keyboard.press('Escape');
  await page.locator('[data-group=session]').click();
  await expect(page.locator('#breakdown-rows tr')).toHaveCount(3);
  await page.reload();
  await expect(page.locator('[data-group=session]')).toHaveAttribute('aria-pressed', 'true');
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await checkFilter('project', 'Website');

  // Breakdown session rows open the session page instead of silently filtering.
  await page.locator('#breakdown-rows a', { hasText: 'session-3' }).click();
  await expect(page).toHaveURL(base + '/sessions/session-3');
  await expect(page.locator('h1')).toHaveText('Session');
  await expect(page.locator('.chip-note')).toBeVisible();
  // Completions are listed per turn. The fixture's usage predates turns, so it forms one
  // untracked turn holding the session's completions and its subagent's.
  const completions = page.locator('#turn-rows .turn-detail tbody tr');
  await page.locator('#turn-rows .turn-toggle').first().click();
  await expect(completions).toHaveCount(5);
  await expect(page.locator('#events')).toHaveCount(0);
  await expect(page.locator('#subagent-tree')).toContainText('sub-3a');
  // No hook ran for the fixture, so the page explains how billing modes get recorded.
  await expect(page.locator('#billing-source')).toContainText('meta-cost hook install');
  fs.appendFileSync(path.join(logs, 'session-3', 'session.jsonl'), jsonl([event('new-live', 'session-3', 1234, 100, 'muse-spark-1.3')]));
  await expect(completions).toHaveCount(6, { timeout: 10_000 });
  await page.reload();
  await expect(page.locator('h1')).toHaveText('Session');
  await page.locator('#turn-rows .turn-toggle').first().click();
  await expect(completions).toHaveCount(6);
  const downloading = page.waitForEvent('download');
  await page.locator('#export').click();
  const exported = JSON.parse(fs.readFileSync(await (await downloading).path(), 'utf8'));
  expect(exported).toHaveLength(6);
  expect(new Set(exported.map((row) => row.session))).toEqual(new Set(['session-3', 'sub-3a']));
  // Turns: earlier usage has no turn; a new prompt and its completion form one, with the prompt
  // read from the log and the subagent counted in the turn it ran during.
  await expect(page.locator('#turn-rows')).toContainText('Usage recorded before turns were tracked');
  await expect(page.locator('#turn-rows .turn-detail')).toContainText('Subagent sub-3a');
  await page.locator('#turn-rows .turn-toggle').first().click();
  const prompt = { id: 'prompt-1', stream: { id: 'session-3' }, sequence: 2, recorded_at: Date.now() * 1000, payload_type: 'runtime.user_intent.accepted',
    payload: { intent_id: 'turn-1', refill_blocks: [{ kind: 'text', text: 'Summarise the README for me' }] } };
  const answer = event('turn-event', 'session-3', 0, 0, 'muse-spark-1.3');
  answer.payload.run_id = 'turn-1';
  fs.appendFileSync(path.join(logs, 'session-3', 'session.jsonl'), jsonl([prompt, answer]));
  await expect(page.locator('#turn-rows')).toContainText('Summarise the README for me', { timeout: 10_000 });
  await expect(page.locator('#turns thead')).toContainText('Duration');
  await expect(page.locator('#turn-rows .turn-row').last().locator('td').nth(2)).toHaveText(/^\d+s$/);
  await page.locator('#turn-rows .turn-toggle').last().click();
  await expect(page.locator('#turn-rows .turn-detail tbody tr')).toHaveCount(1);
  await expect(page.locator('#turn-rows .turn-row').last()).toContainText('—');
  await page.locator('#subagent-tree a', { hasText: 'sub-3a' }).click();
  await expect(page).toHaveURL(base + '/sessions/sub-3a');
  await page.locator('#turn-rows .turn-toggle').first().click();
  await expect(completions).toHaveCount(2);
  await page.locator('#parent-link').click();
  await expect(page).toHaveURL(base + '/sessions/session-3');
  await page.goBack();
  await expect(page).toHaveURL(base + '/sessions/sub-3a');
  await page.goForward();
  await expect(page).toHaveURL(base + '/sessions/session-3');

  // Sessions list: one row per root session; filter chips explain and undo narrowing.
  await page.locator('.nav', { hasText: 'Sessions' }).click();
  await expect(page.locator('#session-rows tr')).toHaveCount(2);
  await expect(page.locator('#session-rows')).not.toContainText('sub-3a');
  await page.getByRole('button', { name: 'Remove project filter' }).click();
  await expect(page.locator('#session-rows tr')).toHaveCount(4);
  await page.locator('#session-rows tr', { hasText: 'Research' }).locator('td').nth(2).click();
  await expect(page).toHaveURL(base + '/sessions/session-2');
  await page.locator('.breadcrumb a', { hasText: 'Sessions' }).click();
  await openFilters();
  await page.locator('[name=session]').fill('No such session');
  await expect(page.locator('#session-rows')).toContainText('No sessions match');
  await page.locator('#clear').click();
  await expect(page.locator('#session-rows tr')).toHaveCount(4);
  await closeFilters();

  // Naming and labelling a session; its label filters every page and groups the breakdown.
  await page.locator('#session-rows tr', { hasText: 'Research' }).locator('td').nth(2).click();
  await page.locator('#edit-session-label').click();
  await page.locator('#session-label-form [name=name]').fill('Draft the pricing page');
  await page.locator('#session-label-form [name=label]').fill('pricing-work');
  await page.locator('#session-label-form').getByRole('button', { name: 'Save' }).click();
  await expect(page.locator('#session-name')).toHaveText('Draft the pricing page');
  await page.locator('#session-label').click();
  await expect(page).toHaveURL(base + '/sessions');
  await expect(page.locator('#session-rows tr')).toHaveCount(1);
  await expect(page.locator('#session-rows')).toContainText('Draft the pricing page');
  await expect(page.locator('#session-rows')).toContainText('pricing-work');
  await page.getByRole('button', { name: 'Remove label filter' }).click();
  await expect(page.locator('#session-rows tr')).toHaveCount(4);
  await page.locator('.nav', { hasText: 'Breakdown' }).click();
  await page.locator('[data-group=label]').click();
  await expect(page.locator('#breakdown-rows tr')).toHaveCount(2);
  await expect(page.locator('#breakdown-rows')).toContainText('unlabelled');
  await page.locator('[data-group=session]').click();
  await page.locator('.nav', { hasText: 'Projects' }).click();
  await page.locator('#project-rows a', { hasText: 'Website' }).click();
  await expect(page).toHaveURL(base + '/projects/Website');
  await expect(page.locator('#session-count')).toHaveText('2');
  await expect(input).toHaveText('151,234');

  // Chart toggle: cost by default, tokens on request. All the usage is from today, so the chart
  // starts in 15-minute blocks with the whole day on the axis and now marked.
  await page.locator('.nav', { hasText: 'Overview' }).click();
  await expect(page.locator('#chart')).toHaveAttribute('aria-label', 'Estimated cost by 15 minutes');
  await expect(page.locator('#chart .cost-bar')).toHaveCount(96);
  await expect(page.locator('#chart .now-marker')).toHaveCount(1);
  await page.locator('[data-metric=tokens]').click();
  await expect(page.locator('#chart')).toHaveAttribute('aria-label', 'Token usage by 15 minutes');
  await page.locator('[data-metric=cost]').click();
  await page.locator('[data-resolution=hour]').click();
  await expect(page.locator('#chart')).toHaveAttribute('aria-label', 'Estimated cost by hour');
  await expect(page.locator('#chart .cost-bar')).toHaveCount(24);
  await page.locator('[data-resolution=day]').click();
  await expect(page.locator('#chart')).toHaveAttribute('aria-label', 'Estimated cost by day');
  await page.reload();
  await expect(page.locator('#chart')).toHaveAttribute('aria-label', 'Estimated cost by day');
  await page.locator('[data-resolution="15m"]').click();
  await expect(page.locator('#chart')).toHaveAttribute('aria-label', 'Estimated cost by 15 minutes');

  // Alerts: a tiny daily limit is exceeded at once and raises the banner on every page.
  // A stub records desktop notifications, as if permission had been granted.
  await page.evaluate(() => {
    window.__notes = [];
    window.Notification = class {
      static permission = 'granted';
      static requestPermission() { return Promise.resolve('granted'); }
      constructor(title) { window.__notes.push(title); }
    };
  });
  await page.locator('.nav', { hasText: 'Alerts' }).click();
  await expect(page.locator('#notification-state')).toContainText('notifications are on');
  await expect(filtersButton()).toHaveCount(0);
  // The form opens in a modal. A click outside it leaves it open, so a stray click cannot
  // discard the form; Escape closes it without saving.
  await page.locator('#add-alert').click();
  await expect(page.getByRole('dialog', { name: 'Add alert' })).toBeVisible();
  await page.mouse.click(5, 5);
  await expect(page.getByRole('dialog', { name: 'Add alert' })).toBeVisible();
  await page.keyboard.press('Escape');
  await expect(page.getByRole('dialog')).toHaveCount(0);
  await expect(page.locator('#add-alert')).toBeFocused();
  await page.locator('#add-alert').click();
  // Warning 1 is always on; extra warnings are disabled until their box is checked.
  await expect(page.locator('#warning-1')).toBeDisabled();
  await page.getByLabel('Use warning 2').check();
  await expect(page.locator('#warning-1')).toBeEnabled();
  await page.locator('#warning-1').fill('60');
  await page.locator('#alert-form [name=limit_usd]').fill('0.01');
  await page.locator('#alert-form').getByRole('button', { name: 'Add alert' }).click();
  await expect(page.getByRole('dialog')).toHaveCount(0);
  const alertRows = page.locator('#alert-rows');
  await expect(page.locator('#alert-rows li')).toHaveCount(1);
  await expect(alertRows).toContainText('Daily limit for all projects');
  await expect(alertRows).toContainText('warns at 60%, 80%');
  const banner = page.locator('#alert-banner');
  await expect(banner).toContainText('Cost limit exceeded');
  await page.locator('.nav', { hasText: 'Sessions' }).click();
  await expect(banner).toBeVisible();
  await page.waitForFunction(() => window.__notes.length === 1);
  await page.waitForTimeout(2500); // later polls must not notify again for the same window and level
  expect(await page.evaluate(() => window.__notes)).toEqual(['Muse Cost: limit exceeded']);
  await page.locator('.nav', { hasText: 'Alerts' }).click();
  // Edit opens the same modal pre-filled; saving updates the alert in place.
  await page.getByRole('button', { name: 'Edit' }).click();
  await expect(page.getByRole('dialog', { name: 'Edit alert' })).toBeVisible();
  await expect(page.locator('#alert-form [name=limit_usd]')).toHaveValue('0.01');
  await expect(page.locator('#warning-0')).toHaveValue('60');
  await expect(page.getByLabel('Use warning 2')).toBeChecked();
  await expect(page.getByLabel('Use warning 3')).not.toBeChecked();
  await page.locator('#alert-form [name=period]').selectOption('week');
  await page.getByRole('button', { name: 'Save alert' }).click();
  await expect(alertRows).toContainText('Weekly limit for all projects');
  await expect(page.locator('#alert-rows li')).toHaveCount(1);
  await page.getByRole('button', { name: 'Pause' }).click();
  await expect(banner).toHaveCount(0, { timeout: 5000 });
  // Preview shows a sample bar without saving anything; it can be ended at once.
  await page.locator('#preview-warning').click();
  await expect(banner).toContainText('Preview');
  await expect(banner).toContainText('Approaching a cost limit');
  await page.getByRole('button', { name: 'End preview' }).click();
  await expect(banner).toHaveCount(0);
  await page.locator('#test-notification').click();
  expect((await page.evaluate(() => window.__notes)).at(-1)).toBe('Muse Cost: test notification');
  page.once('dialog', (dialog) => dialog.accept());
  await page.getByRole('button', { name: 'Delete' }).click();
  await expect(page.locator('#alert-rows li')).toHaveCount(0);

  // Project pages offer a pre-filled alert for that project.
  await page.goto(base + '/projects/Research');
  await page.getByRole('link', { name: 'Add alert for this project →' }).click();
  await expect(page.getByRole('dialog', { name: 'Add alert' })).toBeVisible();
  await expect(page.locator('#alert-form [name=project]')).toHaveValue('Research');
  await page.getByRole('button', { name: 'Cancel' }).click();
  await page.reload();
  await expect(page.locator('#add-alert')).toBeVisible();
  await expect(page.getByRole('dialog')).toHaveCount(0); // the pre-filled dialog opens once, not on refresh

  // Pricing: a local rate overrides the public one and changes the estimate at once.
  await page.locator('.nav', { hasText: 'Pricing' }).click();
  const row = page.locator('#pricing-rows tr', { hasText: 'muse-spark-1.3' });
  await expect(row).toContainText('Public rate');
  await row.getByRole('button', { name: 'Override' }).click();
  for (const name of ['input_per_million', 'cached_input_per_million', 'output_per_million']) {
    await page.locator(`.rate-form [name=${name}]`).fill('0');
  }
  await page.getByRole('button', { name: 'Save local rate' }).click();
  await expect(row).toContainText('Local rate');
  await expect(row).toContainText('$0.00');
  await row.getByRole('button', { name: 'Remove' }).click();
  await expect(row).toContainText('Public rate');
  await page.locator('.nav', { hasText: 'Overview' }).click();
  await expect(input).toHaveText('301,234');
  await page.screenshot({ path: path.join(os.tmpdir(), 'meta-cost-react-overview.png'), fullPage: true });

  await page.setViewportSize({ width: 390, height: 844 });
  await openFilters();
  await expect(page.locator('[name=session]')).toBeVisible();
  await project.selectOption('Research');
  await expect(input).toHaveText('90,000');
  await project.selectOption('Website');
  await expect(input).toHaveText('151,234');
  await page.screenshot({ path: path.join(os.tmpdir(), 'meta-cost-filters-mobile.png'), fullPage: true });
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  await page.keyboard.press('Escape');
  await page.locator('.nav', { hasText: 'Breakdown' }).click();
  await page.getByRole('button', { name: 'Token share' }).click();
  await expect(page.getByRole('tooltip')).toBeVisible();
  const box = await page.getByRole('tooltip').boundingBox();
  expect(box.x).toBeGreaterThanOrEqual(0);
  expect(box.x + box.width).toBeLessThanOrEqual(390);

  // Legacy URLs: query-string filters become state, and old pages redirect.
  await page.goto(base + '/breakdown?period=all&project=Website&model=&from=&to=');
  await expect(page).toHaveURL(base + '/breakdown');
  await checkFilter('project', 'Website');
  await expect(page.locator('#breakdown-rows tr')).toHaveCount(3);
  await page.goto(base + '/activity');
  await expect(page).toHaveURL(base + '/sessions');
  await page.goto(base + '/sessions/no-such-session');
  await expect(page.getByRole('heading', { name: 'Session not found' })).toBeVisible();
  // A period with no usage costs $0.00, not "Unknown".
  await page.goto(base + '/?period=all&from=2000-01-01&to=2000-01-02');
  await expect(page.locator('#cost')).toHaveText('$0.00');
  await expect(page.locator('#daily-cost')).toHaveText('$0.00');
  expect(errors).toEqual([]);
});
