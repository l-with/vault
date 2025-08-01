/**
 * Copyright (c) HashiCorp, Inc.
 * SPDX-License-Identifier: BUSL-1.1
 */

import { module, test } from 'qunit';
import { setupApplicationTest } from 'ember-qunit';
import { setupMirage } from 'ember-cli-mirage/test-support';
import { create } from 'ember-cli-page-object';
import { fillIn, settled, findAll, click } from '@ember/test-helpers';
import { v4 as uuidv4 } from 'uuid';

import enablePage from 'vault/tests/pages/settings/auth/enable';
import page from 'vault/tests/pages/settings/auth/configure/section';
import indexPage from 'vault/tests/pages/settings/auth/configure/index';
import consolePanel from 'vault/tests/pages/components/console/ui-panel';
import { login } from 'vault/tests/helpers/auth/auth-helpers';

import { GENERAL } from 'vault/tests/helpers/general-selectors';

const cli = create(consolePanel);

module('Acceptance | settings/auth/configure/section', function (hooks) {
  setupApplicationTest(hooks);
  setupMirage(hooks);

  hooks.beforeEach(function () {
    this.uid = uuidv4();
    return login();
  });

  test('it can save options', async function (assert) {
    assert.expect(6);
    this.server.post(`/sys/mounts/auth/:path/tune`, function (schema, request) {
      const body = JSON.parse(request.requestBody);
      const keys = Object.keys(body);
      assert.strictEqual(body.token_type, 'batch', 'passes new token type');
      assert.true(keys.includes('default_lease_ttl'), 'passes default_lease_ttl on tune');
      assert.true(keys.includes('max_lease_ttl'), 'passes max_lease_ttl on tune');
      assert.true(keys.includes('description'), 'passes updated description on tune');
      return request.passthrough();
    });
    const path = `approle-save-${this.uid}`;
    const type = 'approle';
    const section = 'options';
    await enablePage.enable(type, path);
    await page.visit({ path, section });
    await fillIn(GENERAL.inputByAttr('description'), 'This is Approle!');
    assert
      .dom(GENERAL.inputByAttr('config.tokenType'))
      .hasValue('default-service', 'as default the token type selected is default-service.');
    await fillIn(GENERAL.inputByAttr('config.tokenType'), 'batch');
    await click(GENERAL.submitButton);
    assert.strictEqual(
      page.flash.latestMessage,
      `The configuration was saved successfully.`,
      'success flash shows'
    );
  });

  for (const type of ['aws', 'azure', 'gcp', 'github', 'kubernetes']) {
    test(`it shows tabs for auth method: ${type}`, async function (assert) {
      const path = `${type}-showtab-${this.uid}`;
      await cli.toggle();
      await settled();
      await cli.consoleInput(`write sys/auth/${path} type=${type}`);
      await cli.enter();
      await indexPage.visit({ path });
      // aws has 4 tabs, the others will have 'Configuration' and 'Method Options' tabs
      const numTabs = type === 'aws' ? 4 : 2;
      const tabs = findAll(GENERAL.linkTo('auth-tab'));
      assert.strictEqual(tabs.length, numTabs, 'shows correct number of tabs');
    });
  }
});
