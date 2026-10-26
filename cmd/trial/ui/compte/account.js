(() => {
  const $ = id => document.getElementById(id);
  const form = $('account-form');
  if (!form) return;

  const dialog = $('trial-account-dialog');
  const message = $('account-message');
  const password = $('account-password');
  const headerButton = $('trial-account-open');
  const quotaNote = $('trial-quota-info');
  let mode = 'signup';
  let account = '';
  let refreshSequence = 0;

  function setMessage(value, error = false) {
    message.textContent = value;
    message.classList.toggle('error', error);
    message.setAttribute('role', error ? 'alert' : 'status');
  }

  function setMode(value) {
    mode = value === 'login' ? 'login' : 'signup';
    $('tab-signup').setAttribute('aria-selected', String(mode === 'signup'));
    $('tab-login').setAttribute('aria-selected', String(mode === 'login'));
    $('account-submit').textContent = mode === 'signup' ? 'Créer mon compte gratuit' : 'Me connecter';
    password.autocomplete = mode === 'signup' ? 'new-password' : 'current-password';
    password.minLength = mode === 'signup' ? 10 : 1;
    $('password-hint').hidden = mode !== 'signup';
    password.value = '';
    setMessage('');
  }

  function showAccount(name) {
    $('current-name').textContent = name;
    $('account-forms').hidden = true;
    $('account-signed-in').hidden = false;
    password.value = '';
  }

  function showForms() {
    $('account-signed-in').hidden = true;
    $('account-forms').hidden = false;
    form.reset();
    setMessage('');
  }

  function quotaText(data) {
    if (data.global_available === false) return 'L’essai IA est complet pour aujourd’hui. Le catalogue reste gratuit.';
    const remaining = Number.isInteger(data.remaining) ? data.remaining : null;
    if (data.authenticated) {
      return remaining === null
        ? 'Compte gratuit connecté · 5 demandes IA par jour.'
        : `Compte ${data.account} · ${remaining} demande${remaining > 1 ? 's' : ''} IA restante${remaining > 1 ? 's' : ''} aujourd’hui sur 5.`;
    }
    return remaining === null
      ? 'Gratuit · 2 demandes IA par jour sans compte. Un cookie nécessaire limite les abus.'
      : `Gratuit · ${remaining} demande${remaining > 1 ? 's' : ''} IA restante${remaining > 1 ? 's' : ''} aujourd’hui sans compte. Un cookie nécessaire limite les abus.`;
  }

  function applySession(data) {
    const nextAccount = data.authenticated && typeof data.account === 'string' ? data.account : '';
    if (nextAccount !== account) {
      account = nextAccount;
      if (account) showAccount(account);
      else showForms();
    }
    if (headerButton) {
      headerButton.textContent = account ? 'Mon compte' : 'Connexion / inscription';
      headerButton.setAttribute('aria-label', account ? 'Ouvrir mon compte' : 'Se connecter ou créer un compte gratuit');
    }
    if (quotaNote) quotaNote.textContent = quotaText({...data, authenticated: !!account, account});
    if (account && $('trial-signup')) $('trial-signup').hidden = true;
  }

  async function request(path, method = 'GET', body) {
    const response = await fetch('/essai/api/' + path, {
      method,
      credentials: 'same-origin',
      headers: body ? {'Content-Type': 'application/json'} : {},
      body: body ? JSON.stringify(body) : undefined
    });
    let data;
    try { data = await response.json(); } catch { data = {}; }
    if (!response.ok || data.status === 'error') throw new Error(data.error || 'Le service est momentanément indisponible.');
    return data;
  }

  async function refresh() {
    const sequence = ++refreshSequence;
    try {
      const data = await request('me');
      if (sequence === refreshSequence) applySession(data);
      return data;
    } catch {
      // The inline quota stays informative without interrupting manual creation.
      return null;
    }
  }

  function open(requestedMode = 'signup') {
    if (!dialog) return;
    if (!account) setMode(requestedMode);
    if (!dialog.open) dialog.showModal();
    (account ? $('account-logout') : $('account-name')).focus();
  }

  window.TrialAccount = {open, refresh};
  headerButton?.addEventListener('click', () => open('login'));
  $('trial-signup-open')?.addEventListener('click', () => open('signup'));
  $('trial-account-close')?.addEventListener('click', () => dialog.close());
  dialog?.addEventListener('close', () => {
    password.value = '';
    setMessage('');
  });
  $('tab-signup').addEventListener('click', () => setMode('signup'));
  $('tab-login').addEventListener('click', () => setMode('login'));

  form.addEventListener('submit', async event => {
    event.preventDefault();
    if (!form.reportValidity()) return;
    const button = $('account-submit');
    button.disabled = true;
    setMessage('Un instant…');
    try {
      const data = await request(mode, 'POST', {
        username: $('account-name').value,
        password: password.value
      });
      ++refreshSequence;
      applySession({authenticated: true, account: data.account || data.username || $('account-name').value, remaining: data.remaining, global_available: true});
      setMessage('');
      if (dialog) dialog.close();
      refresh();
      window.dispatchEvent(new CustomEvent('trial:account-changed', {detail: {authenticated: true}}));
    } catch (error) {
      setMessage(error.message || 'Connexion impossible. Réessayez.', true);
    } finally {
      password.value = '';
      button.disabled = false;
    }
  });

  $('account-logout').addEventListener('click', async () => {
    const button = $('account-logout');
    button.disabled = true;
    setMessage('Déconnexion…');
    try {
      await request('logout', 'POST');
      ++refreshSequence;
      applySession({authenticated: false, remaining: null, global_available: true});
      setMessage('Vous êtes déconnecté.');
      refresh();
      window.dispatchEvent(new CustomEvent('trial:account-changed', {detail: {authenticated: false}}));
    } catch (error) {
      setMessage(error.message || 'Déconnexion impossible. Réessayez.', true);
    } finally {
      button.disabled = false;
    }
  });

  window.addEventListener('focus', () => {
    if (!document.hidden) refresh();
  });
  refresh();
})();
