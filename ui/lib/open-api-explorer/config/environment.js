/* eslint-env node */
'use strict';

module.exports = function (environment) {
  const ENV = {
    modulePrefix: 'open-api-explorer',
    environment,
    APP: {
      NAMESPACE_ROOT_URLS: ['sys/health', 'sys/seal-status', 'sys/license/features'],
    },
  };

  return ENV;
};
