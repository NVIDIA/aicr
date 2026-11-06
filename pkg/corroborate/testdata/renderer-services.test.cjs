// Copyright (c) 2026, NVIDIA CORPORATION & AFFILIATES.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const test = require('node:test');
const html = fs.readFileSync(path.join(__dirname, '../renderer/index.html'), 'utf8');
new vm.Script(html.match(/^<script>\n([^]*?)<\/script>/m)[1]);

// Exercise the shipped functions without booting fetch or requiring a browser.
// This small DOM stand-in records output; browser layout is checked separately.
class Element {
  children = [];
  attributes = {};
  dataset = {};
  classList = { add() {}, toggle() {} };
  innerHTML = '';
  textContent = '';
  appendChild(child) { this.children.push(child); return child; }
  append(...children) { this.children.push(...children); }
  setAttribute(name, value) { this.attributes[name] = value; }
  addEventListener() {}
  querySelector() { return this.part ??= new Element(); }
  querySelectorAll() { return []; }
}
function renderer(index) {
  const elements = {};
  const context = vm.createContext({
    document: {
      createElement: () => new Element(),
      getElementById: id => elements[id] ??= new Element(),
    },
    requestAnimationFrame: fn => fn(),
    currentRecipeIdx: null, criteria: {}, CRIT_AXES: ['service'], SERVICE_ORDER: [],
    PHASES: [], STATE_META: { UNTESTED: { label: 'untested' } }, STATE_COLOR: {},
    collapsedGroups: new Set(), collapsedDashes: new Set(),
    anyCriteria: () => false, recipeMatchesK8s: () => true,
    versionView: recipe => recipe, rollupRecipe: () => ({ state: 'UNTESTED' }),
    syncTreeToggle() {},
  });
  const names = [
    'esc', 'coordParts', 'routeSegs', 'coordPath', 'hashSegs', 'buildModelFromIndex',
    'serviceDisplayName', 'serviceLabel', 'criteriaLabel',
    'renderLandingCards', 'renderLandingEmpty', 'renderTree', 'bindTreeDestination',
    'renderCatalog', 'announceResults', 'recipeMatchesCriteria', 'groupMatchesCriteria',
    'renderLandingRecipeGrid', 'overviewRecipeNameHTML', 'overviewStateCell', 'overviewChip',
  ];
  for (const name of names) {
    const match = html.match(new RegExp(`^function ${name}\\([^\\n]*}$|^function ${name}\\([^]*?^}`, 'm'));
    if (match) vm.runInContext(match[0], context);
  }
  context.buildModelFromIndex(index);
  return { context, elements };
}
function fixture(service, displayName, populated = false) {
  const coord = { service, accelerator: 'h100', os: 'ubuntu', intent: 'training', platform: '' };
  return { sources: {}, criteria: { service: [service] }, groups: [{
    service, ...(displayName === undefined ? {} : { displayName }),
    dashboards: populated ? [{ accelerator: 'h100', os: 'ubuntu', tabs: [{
      recipe: `h100-${service}-ubuntu-training`, coord, versions: [],
    }] }] : [],
  }] };
}
function markup(element) {
  return element.innerHTML + element.children.map(markup).join('') + (element.part ? markup(element.part) : '');
}

for (const [name, id, displayName] of [
  ['full name', 'eks', 'Amazon Elastic Kubernetes Service'],
  ['legacy JSON', 'metal3', undefined],
  ['empty name', 'nvke', ''],
  ['brand spelling', 'kind', 'kind'],
  ['k0s spelling', 'k0s', 'k0s'],
  ['wildcard', 'any', 'Any service'],
  ['empty ID', '', undefined],
  ['custom ID', 'Custom-Service-With-A-Very-Long-Exact-ID', undefined],
  ['custom name', 'custom', 'A very long custom service display name that must remain complete'],
  ['hostile ID', '<img src=x onerror="alert(1)">&\'custom', undefined],
  ['hostile name', 'custom', '<script>alert("x")</script>&\''],
]) {
  test(name, () => {
    const { context: c, elements } = renderer(fixture(id, displayName));
    const label = displayName || id;
    const full = label === id ? id : `${label} (${id})`;
    const host = new Element();
    c.renderLandingCards(host);
    const card = markup(host);
    assert.ok(card.includes(c.esc(label)), card);
    assert.ok(card.includes(c.esc(id)), card);
    assert.ok(card.indexOf(c.esc(label)) <= card.lastIndexOf(c.esc(id)));
    assert.ok(!card.includes('<script>') && !card.includes('<img'));
    assert.equal(elements['c-service'].children[1].value, id);
    assert.equal(elements['c-service'].children[1].textContent, full);
    assert.equal(c.TREE[0].name, id);
    c.renderTree();
    const tree = markup(elements['tree-host']);
    assert.ok(tree.includes(c.esc(label)) && tree.includes(c.esc(id)), tree);
    const toggle = elements['tree-host'].children[0].children[0].children[0];
    assert.equal(toggle.attributes['aria-label'], `collapse ${full} dashboards`);
    c.renderCatalog();
    assert.equal(elements['cat-group-name'].textContent, full);
    assert.ok(elements['catalog-crumb'].innerHTML.includes(c.esc(full)));
    assert.ok(markup(elements['catalog-host']).includes(c.esc(full)));
    assert.ok(!markup(elements['catalog-host']).includes('<script>'));
    assert.equal(c.hashSegs(id), '#/' + encodeURIComponent(id));
    const populated = renderer(fixture(id, displayName, true));
    const rows = new Element();
    populated.context.renderLandingRecipeGrid(rows);
    const rowHTML = markup(rows);
    assert.ok(rowHTML.includes(c.esc(label)) && rowHTML.includes(c.esc(id)), rowHTML);
    assert.ok(!rowHTML.includes('<script>') && !rowHTML.includes('<img'));
    const rowLink = rows.children[0].children[0].part.children[0].children[0].children[0];
    assert.equal(rowLink.attributes['aria-label'], `open ${full} dashboard`);
  });
}

test('populated service rows retain labels, IDs and recipe routes', () => {
  const id = 'mk8s', name = 'Nebius Managed Service for Kubernetes';
  const { context: c, elements } = renderer(fixture(id, name, true));
  const host = new Element();
  c.renderLandingRecipeGrid(host);
  const row = markup(host);
  assert.ok(row.includes(name), row);
  assert.ok(row.indexOf(name) < row.indexOf('>mk8s<'), row);
  assert.equal(c.COORD_TO_IDX['mk8s/h100-ubuntu/training'], 0);
  assert.equal(c.DATA.recipes[0].coord.service, id);
  c.renderTree();
  const destination = elements['tree-host'].children[0].children[0].children[1];
  assert.equal(destination.href, '#/mk8s');
  assert.equal(destination.attributes['aria-label'], `open ${name} (${id}) dashboard`);
});

test('empty index renders without service assumptions', () => {
  const { context: c } = renderer({ sources: {}, criteria: {}, groups: [] });
  const host = new Element();
  c.renderLandingCards(host);
  c.renderTree();
  assert.ok(markup(host).includes('No recipes match'));
  assert.equal(c.currentGroup, null);
});
