const { test } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
const vm = require('node:vm');

const login = readFileSync(join(__dirname, '../cmd/cline-proxy/web/login.html'), 'utf8');
const admin = readFileSync(join(__dirname, '../cmd/cline-proxy/web/admin.html'), 'utf8');
const scripts = html => [...html.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/g)].map(m => m[1]);

function page(html, storedTheme) {
  const attrs = new Map([['data-theme', 'dark']]);
  const listeners = new Map();
  const storage = new Map();
  if (storedTheme !== undefined) storage.set('cline-admin-theme', storedTheme);
  const elements = Object.fromEntries(['themeToggle', 'themeLabel', 'themeIcon'].map(id => [id, {
    addEventListener(event, handler) { listeners.set(event, handler); },
  }]));
  const root = {
    setAttribute: (key, value) => attrs.set(key, value),
    removeAttribute: key => attrs.delete(key),
    getAttribute: key => attrs.get(key),
    classList: { add() {}, remove() {} },
  };
  const ctx = vm.createContext({
    document: { documentElement: root, getElementById: id => elements[id] },
    localStorage: { getItem: key => storage.get(key) ?? null, setItem: (key, value) => storage.set(key, value) },
    window: { addEventListener(event, handler) { listeners.set(event, handler); } },
    setTimeout: handler => handler(),
  });
  vm.runInContext(scripts(html)[0], ctx);
  return { attrs, storage, listeners, elements, ctx };
}

test('login and admin default dark, and saved light takes precedence', () => {
  for (const saved of [undefined, 'light', 'dark']) {
    const loginPage = page(login, saved);
    const adminPage = page(admin, saved);
    vm.runInContext(scripts(admin).at(-1), adminPage.ctx);
    const expected = saved === 'light' ? undefined : 'dark';
    assert.equal(loginPage.attrs.get('data-theme'), expected);
    assert.equal(adminPage.attrs.get('data-theme'), expected);
    assert.equal(adminPage.elements.themeLabel.textContent, expected === 'dark' ? '浅色' : '深色');
  }
  assert.ok(login.indexOf('<script>') < login.indexOf('<style>'));
  assert.ok(admin.indexOf('<script>') < admin.indexOf('<style>'));
});

test('a theme chosen in admin is used on login, including an open login tab', () => {
  const adminPage = page(admin);
  vm.runInContext(scripts(admin).at(-1), adminPage.ctx);
  adminPage.listeners.get('click')();
  assert.equal(adminPage.storage.get('cline-admin-theme'), 'light');
  assert.equal(page(login, adminPage.storage.get('cline-admin-theme')).attrs.get('data-theme'), undefined);

  const loginPage = page(login);
  loginPage.listeners.get('storage')({key:'cline-admin-theme', newValue:'light'});
  assert.equal(loginPage.attrs.get('data-theme'), undefined);
  loginPage.listeners.get('storage')({key:'cline-admin-theme', newValue:'dark'});
  assert.equal(loginPage.attrs.get('data-theme'), 'dark');
});
