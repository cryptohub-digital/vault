import Route from '@ember/routing/route';
import UnloadModelRoute from 'vault/mixins/unload-model-route';
import UnsavedModelRoute from 'vault/mixins/unsaved-model-route';
import { inject as service } from '@ember/service';

export default Route.extend(UnloadModelRoute, UnsavedModelRoute, {
  store: service(),
  // intentionally blank - we don't want a model until one is
  // created via the form in the controller
  model() {
    return {};
  },
  activate() {
    this.store.unloadAll('secret-engine');
  },
});
