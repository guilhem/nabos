'use strict';
(() => {
  const page = document.getElementById('wifi');
  const transition = document.getElementById('wifi-transition');
  if (!page && !transition) return;
  const status = document.getElementById('wifi-status');
  const authorization = document.getElementById('wifi-authorization');
  const labels = {idle: 'Prêt', connecting: 'Connexion en cours', succeeded: 'Connexion réussie', failed: 'Connexion échouée : vérifiez le mot de passe et le réseau', cancelled: 'Tentative annulée'};
  let switching = Boolean(transition);
  async function poll() {
    try {
      const response = await fetch('/wifi/status', {cache: 'no-store'});
      if (!response.ok || response.redirected) throw new Error('unreachable');
      const data = await response.json();
      const s = data.status;
      if (transition && String(s.attempt_id) !== transition.dataset.attempt) return;
      status.textContent = (labels[s.phase] || 'Reconnexion en cours') + (s.address ? ' — ' + s.address : '');
      if (authorization) authorization.textContent = data.authorized ? 'Confirmation physique reçue.' : 'Appuyez sur le bouton du lapin pour autoriser cette tentative pendant cinq minutes.';
      document.querySelectorAll('[data-wifi-connect]').forEach(button => {
        button.disabled = s.phase === 'connecting' || (page.dataset.setup === 'true' && !data.authorized);
      });
      if (s.phase === 'succeeded' && s.mode === 'client') {
        switching = true;
        status.textContent += ' — Retrouvez le lapin sur votre réseau.';
      }
    } catch (_) {
      if (switching) status.textContent = 'La page a perdu le lapin pendant la bascule. Retrouvez-le sur votre réseau ; en cas d’échec, rejoignez à nouveau son hotspot.';
      else {
        status.textContent = 'Le lapin est temporairement inaccessible. Rechargez la page pour réessayer.';
        if (page && page.dataset.setup === 'true') document.querySelectorAll('[data-wifi-connect]').forEach(button => { button.disabled = true; });
      }
    }
  }
  async function renew() {
    if (document.hidden || switching) return;
    try {
      const response = await fetch('/wifi/reserve', {method: 'POST', headers: {Accept: 'application/json'}});
      if (!response.ok) throw new Error('reservation');
      await poll();
    } catch (_) {
      status.textContent = 'Réservation indisponible. Rechargez la page ou utilisez « Préparer la connexion ».';
    }
  }
  if (page) {
    const networks = document.getElementById('wifi-network');
    const security = document.getElementById('wifi-security');
    const password = document.getElementById('wifi-password');
    const manual = document.getElementById('wifi-ssid');
    function passwordMode() {
      password.disabled = security.value === 'open';
      if (password.disabled) password.value = '';
    }
    networks.addEventListener('change', () => {
      const selected = networks.selectedOptions[0];
      if (selected.value) security.value = selected.dataset.security;
      manual.disabled = Boolean(selected.value);
      manual.required = !selected.value;
      passwordMode();
    });
    security.addEventListener('change', passwordMode);
    document.querySelectorAll('form[action="/wifi/connect"], form[action="/wifi/release"]').forEach(form => form.addEventListener('submit', () => { switching = true; }));
    if (page.dataset.mode === 'hotspot') {
      renew();
      setInterval(renew, 30000);
    }
    passwordMode();
  }
  setInterval(poll, 3000);
})();
