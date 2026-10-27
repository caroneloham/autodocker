from pathlib import Path
import shutil
import re

import argparse

root = Path(__file__).resolve().parents[1]
parser = argparse.ArgumentParser(description='Assembler l’interface de l’essai public AutoDocker.')
parser.add_argument('--tracer-dir', type=Path, default=root.parent / 'docker-tracer', help='Dossier des sources Docker Tracer, avec dist-browser déjà construit.')
args = parser.parse_args()
source = root
assets = source / 'cmd' / 'trial' / 'ui'
assets.mkdir(parents=True, exist_ok=True)
tracer_build = args.tracer_dir.resolve() / 'dist-browser'
logo_assets = list((tracer_build / 'assets').glob('autodocker-logo-*.png'))
if len(logo_assets) != 1:
    raise SystemExit('Build Docker Tracer browser assets with the AutoDocker logo first')
logo_href = '/essai/tracer/assets/' + logo_assets[0].name
logo_markup = f'<span class="autodocker-logo" aria-hidden="true"><img src="{logo_href}" alt="" draggable="false"></span>'

def replace_once(source, before, after):
    if source.count(before) != 1:
        raise ValueError(f'Expected one trial asset anchor: {before[:70]}')
    return source.replace(before, after, 1)

html = (source / 'lab-ui' / 'index.html').read_text(encoding='utf-8')
html = html.replace('href="/favicon.svg"', 'href="/essai/favicon.svg"')
html = html.replace('href="/workspace.css"', 'href="/essai/workspace.css"')
html = html.replace('src="/app.js"', 'src="/essai/app.js"')
html = replace_once(html, '<script src="/essai/app.js" defer></script>', '<script src="/essai/app.js" defer></script><script src="/essai/compte/account.js" defer></script>')
html = html.replace('href="/" aria-label="AutoDocker, accueil"', 'href="/essai/" aria-label="AutoDocker, essai"')
brand_icon = re.search(r'(?<=aria-label="AutoDocker, essai">)<svg.*?</svg>', html)
if not brand_icon:
    raise ValueError('AutoDocker brand icon not found')
html = replace_once(html, brand_icon.group(), logo_markup)
html = html.replace('<link rel="icon" href="/essai/favicon.svg" type="image/svg+xml">', f'<link rel="icon" href="{logo_href}" type="image/png">')
html = html.replace('<span class="local-label">Sur votre ordinateur</span>', '<span class="local-label">Essai public</span>')
html = html.replace('<a href="/compte" class="quiet-link">Mon compte</a><a href="/logout" class="quiet-link">Déconnexion</a>', '<button id="trial-account-open" type="button" class="quiet-link trial-account-trigger">Connexion / inscription</button><a href="/essai/tracer/" class="quiet-link">Docker Tracer</a>')
html = replace_once(html, '<footer><span>AutoDocker</span><span>Votre infrastructure, sur votre ordinateur.</span></footer>', '<footer><span>AutoDocker</span><span>Votre infrastructure, sur votre ordinateur.</span><span class="trial-local-downloads"><a class="trial-local-download" href="https://github.com/caroneloham/autodocker" target="_blank" rel="noopener noreferrer">Télécharger sur GitHub ↗</a></span></footer>')
html = replace_once(html, '<section id="ai-panel" class="ai-panel" hidden aria-label="Créer une infrastructure avec un prompt">', '<section id="ai-panel" class="ai-panel" hidden aria-label="Créer une infrastructure avec un prompt"><p id="trial-quota-info" class="option-note">Gratuit · 2 demandes IA par jour sans compte. Un cookie nécessaire limite les abus.</p>')
html = replace_once(html, 'id="infra-prompt" rows="3" maxlength="8000" aria-describedby="ai-progress"', 'id="infra-prompt" rows="3" aria-describedby="ai-progress trial-prompt-count"')
html = replace_once(html, '</textarea><div class="prompt-examples"', '</textarea><p id="trial-prompt-count" class="trial-prompt-count" aria-live="polite">0 / 2 000 caractères</p><div class="prompt-examples"')
html = replace_once(html, '<p id="ai-error" class="error-box" role="alert" hidden></p>', '<p id="ai-error" class="error-box" role="alert" hidden></p><section id="trial-signup" class="trial-signup" role="status" hidden><div><p class="eyebrow">Vos 2 essais IA sont utilisés</p><h3>Continuez gratuitement</h3><p>Créez un compte en quelques secondes pour obtenir 5 demandes IA par jour. Votre demande et votre sélection restent sur cette page.</p></div><div class="trial-signup-actions"><button id="trial-signup-open" type="button" class="button primary">Créer mon compte gratuit <span aria-hidden="true">→</span></button><a href="/essai/compte/">Ouvrir la page compte</a></div></section>')
html = replace_once(html, '<details class="code-details"><summary>Voir le fichier Docker Compose</summary><pre id="compose-preview" tabindex="0"></pre></details>', '<details class="code-details"><summary>Aperçu du Compose</summary><p class="trial-compose-note">Les accès définitifs sont créés avec l’archive.</p><pre id="compose-preview" tabindex="0"></pre></details>')
html = replace_once(html, '<p id="download-error" class="error-box" role="alert" hidden></p><details class="start-guide">', '<p id="download-error" class="error-box" role="alert" hidden></p><details class="code-details trial-final-compose"><summary>Voir le Compose final</summary><pre id="compose-final" tabindex="0"></pre></details><details class="start-guide">')
account_dialog = '''
  <dialog id="trial-account-dialog" class="trial-account-dialog" aria-labelledby="trial-account-title">
    <div class="trial-account-heading"><div><p class="eyebrow">Essai gratuit</p><h2 id="trial-account-title">Votre compte AutoDocker</h2></div><button id="trial-account-close" type="button" aria-label="Fermer la fenêtre de connexion">Fermer <span aria-hidden="true">×</span></button></div>
    <div id="account-forms">
      <p class="account-note">Un identifiant et un mot de passe suffisent. Votre projet reste sur cette page.</p>
      <div class="account-tabs" role="tablist" aria-label="Compte"><button type="button" id="tab-signup" role="tab" aria-selected="true" aria-controls="account-form">Créer un compte</button><button type="button" id="tab-login" role="tab" aria-selected="false" aria-controls="account-form">Se connecter</button></div>
      <form id="account-form" autocomplete="off">
        <label for="account-name">Identifiant</label><input id="account-name" name="username" type="text" minlength="3" maxlength="32" pattern="[A-Za-z0-9][A-Za-z0-9_-]{2,31}" autocomplete="username" required>
        <p class="account-hint">3 à 32 caractères : lettres, chiffres, tiret ou soulignement.</p>
        <label for="account-password">Mot de passe</label><input id="account-password" name="password" type="password" minlength="10" maxlength="72" autocomplete="new-password" required>
        <p id="password-hint" class="account-hint">Au moins 10 caractères, 72 octets maximum. Pas de récupération par e-mail dans cet essai.</p>
        <button class="account-submit" id="account-submit" type="submit">Créer mon compte gratuit</button>
      </form>
    </div>
    <div id="account-signed-in" class="account-current" hidden><h3>Vous êtes connecté</h3><p>Compte : <strong id="current-name"></strong></p><p>Reprenez votre demande IA quand vous le souhaitez.</p><button id="account-logout" type="button">Se déconnecter</button></div>
    <p id="account-message" class="account-message" role="status" aria-live="polite"></p>
    <p class="account-privacy">Un cookie de session nécessaire garde votre connexion. Aucun cookie publicitaire. <a href="/essai/compte/">Ouvrir la page compte</a></p>
  </dialog>
'''
html = replace_once(html, '</body>', account_dialog + '</body>')
(assets / 'index.html').write_text(html, encoding='utf-8')

js = (source / 'lab-ui' / 'app.js').read_text(encoding='utf-8')
js = js.replace("'/api/", "'/essai/api/").replace('"/api/', '"/essai/api/')
js = replace_once(js, "const error = new Error(data.error || 'La demande a échoué. Réessayez.'); error.logs = data.logs || ''; throw error;", "const error = new Error(data.error || 'La demande a échoué. Réessayez.'); error.logs = data.logs || ''; error.code = data.code || ''; error.status = response.status; throw error;")
js = replace_once(js, "} catch (error) { if(error.name==='AbortError')throw error;throw new Error(error.message || 'Le fournisseur IA ne répond pas. Vérifiez son API ou utilisez le catalogue.'); }", "} catch (error) { if(error.name==='AbortError'||error.code)throw error;throw new Error(error.message || 'Le fournisseur IA ne répond pas. Vérifiez son API ou utilisez le catalogue.'); }")
js = replace_once(js, "if (prompt.length > 8000) throw new Error('Raccourcissez votre demande à moins de 8 000 caractères.');", "if (Array.from(prompt).length > 2000) throw new Error('Raccourcissez votre demande à 2 000 caractères maximum.');")
js = replace_once(js, "$('infra-prompt').addEventListener('input',()=>{state.aiQuestions=false;", "const trialPromptCount=()=>{$('trial-prompt-count').textContent=Array.from($('infra-prompt').value).length.toLocaleString('fr-FR')+' / 2 000 caractères';};$('infra-prompt').addEventListener('input',()=>{trialPromptCount();state.aiQuestions=false;")
js = replace_once(js, "$('infra-prompt').value+='\\nPrécision : '+answer;", "const combined=$('infra-prompt').value+'\\nPrécision : '+answer;if(Array.from(combined.trim()).length>2000){showError('ai-error','Votre demande dépasse 2 000 caractères. Raccourcissez-la avant de compléter.');$('infra-prompt').focus();return;}$('infra-prompt').value=combined;")
js = replace_once(js, "$('infra-prompt').value='';$('ai-propose').disabled=true;", "$('infra-prompt').value='';trialPromptCount();$('ai-propose').disabled=true;")
js = replace_once(js, "showError('ai-error','');$('ai-proposal').hidden=true", "showError('ai-error','');$('trial-signup').hidden=true;$('ai-proposal').hidden=true")
js = replace_once(js, "}catch(error){if(error.name==='AbortError')$('ai-progress').textContent='Demande annulée. Vous pouvez modifier votre besoin.';else{showError('ai-error',error.message);$('ai-progress').textContent='Le choix manuel reste disponible.';}}", "}catch(error){if(error.name==='AbortError')$('ai-progress').textContent='Demande annulée. Vous pouvez modifier votre besoin.';else if(error.code==='guest_quota_exhausted'){$('trial-signup').hidden=false;$('ai-progress').textContent='Deux demandes IA gratuites utilisées aujourd’hui. Votre demande reste en place.';window.TrialAccount?.open('signup');}else{showError('ai-error',error.message);$('ai-progress').textContent='Le choix manuel reste disponible.';}}")
js = replace_once(js, "finally{aiController=null;$('ai-cancel').hidden=true;state.aiPending=false;updateAIAction();$('ai-propose').classList.remove('busy');$('infra-prompt').disabled=false;$('ai-propose').setAttribute('aria-busy','false');updateSummary();}", "finally{aiController=null;$('ai-cancel').hidden=true;state.aiPending=false;updateAIAction();$('ai-propose').classList.remove('busy');$('infra-prompt').disabled=false;$('ai-propose').setAttribute('aria-busy','false');updateSummary();window.TrialAccount?.refresh();}")
js = replace_once(js, "state.result = result;\n      $('done-description')", "state.result = result;\n      $('compose-preview').textContent = result.compose || '';\n      $('compose-final').textContent = result.compose || '';\n      $('done-description')")
js = replace_once(js, "state.result = null; state.configuration = null;", "state.result = null; $('compose-final').textContent = ''; state.configuration = null;")
(assets / 'app.js').write_text(js, encoding='utf-8')
css = (source / 'lab-ui' / 'workspace.css').read_text(encoding='utf-8')
css = replace_once(css, '.ai-panel > label { margin-top: 28px; font-size: 14px; }', '''.ai-panel > label { margin-top: 28px; font-size: 14px; }
.trial-local-download { color:var(--secondary);text-decoration:none;white-space:nowrap; }
.trial-local-download:hover { color:var(--text); }
.trial-local-downloads { display:inline-flex;gap:14px;align-items:center;flex-wrap:wrap; }
.trial-signup { display:flex; align-items:center; justify-content:space-between; gap:24px; margin-top:22px; padding:22px 24px; border:1px solid var(--blue-line); border-radius:var(--radius); background:var(--surface); }
.trial-signup[hidden] { display:none; }
.trial-signup .eyebrow { margin:0 0 5px; color:var(--blue-light); font-size:11px; letter-spacing:1.4px; text-transform:uppercase; }
.trial-signup h3 { margin:0; font-size:21px; letter-spacing:-.4px; }
.trial-signup p:not(.eyebrow) { margin:6px 0 0; color:var(--secondary); font-size:13px; }
.trial-signup .button { flex-shrink:0; text-decoration:none; white-space:nowrap; }
.trial-signup-actions { display:grid;gap:8px;text-align:center; }
.trial-signup-actions a { color:var(--secondary);font-size:12px; }
.trial-account-trigger { border:0;background:transparent;color:var(--muted);padding:4px 0;font:inherit;white-space:nowrap; }
.trial-account-trigger:hover,.trial-account-trigger:focus-visible { color:var(--text); }
.trial-prompt-count { margin:6px 0 0;color:var(--muted);font-size:12px;text-align:right; }
.trial-compose-note { color:var(--secondary);font-size:13px;margin:12px 0; }
.trial-final-compose { max-width:100%;margin-top:26px;text-align:left; }
.trial-account-dialog { width:min(92vw,500px);max-height:min(92dvh,760px);overflow:auto;margin:auto;padding:28px;border:1px solid var(--line);border-radius:18px;background:var(--surface);color:var(--text);box-shadow:0 28px 90px #0008; }
.trial-account-dialog::backdrop { background:#05080dcc;backdrop-filter:blur(4px); }
.trial-account-dialog [hidden] { display:none; }
.trial-account-heading { display:flex;justify-content:space-between;align-items:flex-start;gap:16px;margin-bottom:18px; }
.trial-account-heading .eyebrow { margin:0 0 5px; }
.trial-account-heading h2 { font-size:25px;line-height:1.15;letter-spacing:-.7px;margin:0; }
.trial-account-heading>button { border:1px solid var(--line);border-radius:8px;background:var(--surface-raised);color:var(--secondary);padding:8px 10px;white-space:nowrap; }
.trial-account-dialog .account-note { margin:0 0 20px;color:var(--secondary);font-size:14px;line-height:1.55; }
.trial-account-dialog .account-tabs { display:grid;grid-template-columns:1fr 1fr;gap:4px;margin-bottom:18px;padding:4px;border-radius:10px;background:var(--surface-input); }
.trial-account-dialog .account-tabs button { border:0;border-radius:7px;background:transparent;color:var(--secondary);padding:10px;font-size:14px;font-weight:600; }
.trial-account-dialog .account-tabs button[aria-selected=true] { background:var(--surface-raised);color:var(--text); }
.trial-account-dialog label { display:block;margin:14px 0 7px;font-size:13px;font-weight:600; }
.trial-account-dialog input { width:100%;min-height:45px;padding:10px 12px;border:1px solid var(--line);border-radius:8px;background:var(--surface-input);color:var(--text);font:inherit; }
.trial-account-dialog input:focus-visible { outline:2px solid var(--blue-light); }
.trial-account-dialog .account-hint { margin:7px 0 0;color:var(--muted);font-size:12px;line-height:1.45; }
.trial-account-dialog .account-submit { width:100%;margin-top:20px;padding:13px 20px;border:0;border-radius:8px;background:var(--blue);color:#fff;font-weight:700; }
.trial-account-dialog .account-submit:disabled { opacity:.6; }
.trial-account-dialog .account-message { min-height:20px;margin:14px 0 0;color:var(--secondary);font-size:13px; }
.trial-account-dialog .account-message.error { color:#ffaaa6; }
.trial-account-dialog .account-current { padding:18px;border:1px solid var(--blue-line);border-radius:10px;background:var(--selected); }
.trial-account-dialog .account-current h3 { margin:0;font-size:19px; }
.trial-account-dialog .account-current p { margin:8px 0;color:var(--secondary);font-size:14px; }
.trial-account-dialog .account-current button { margin-top:9px;padding:10px 14px;border:1px solid var(--line);border-radius:8px;background:var(--surface-raised);color:var(--text); }
.trial-account-dialog .account-privacy { margin:18px 0 0;padding-top:16px;border-top:1px solid var(--line-soft);color:var(--muted);font-size:12px;line-height:1.55; }
.trial-account-dialog .account-privacy a { color:var(--blue-light); }
@media(max-width:680px) { .trial-signup { align-items:stretch;flex-direction:column;padding:20px; } .trial-signup-actions .button { width:100%;white-space:normal; } .trial-account-dialog { width:calc(100vw - 24px);padding:20px;max-height:calc(100dvh - 24px); } .header-right { grid-column:1/-1;order:2;width:100%;justify-content:space-between;flex-wrap:wrap; } .header-right>a:last-child { margin-left:auto; } .steps { order:3; } }''')
css += '''
.autodocker-logo { position:relative;display:inline-block;width:52px;height:52px;overflow:hidden;flex:0 0 auto; }
.autodocker-logo img { position:absolute;width:160%;max-width:none;height:auto;left:-26.4%;top:-41.1%; }
.account-brand { display:inline-flex;align-items:center;gap:10px; }
.account-brand .autodocker-logo { width:42px;height:42px; }
@media(min-width:1200px) { .app-header { grid-template-columns:minmax(210px,1fr) auto auto;gap:22px; } .header-right { gap:18px; } .header-right>.quiet-link { white-space:nowrap; } }
@media(min-width:681px) and (max-width:1199px) { .app-header { grid-template-columns:1fr auto;padding-top:20px; } .steps { grid-column:1/-1;order:3;justify-content:center;padding-bottom:17px; } .header-right>.quiet-link { white-space:nowrap; } }
@media(max-width:680px) { .brand .autodocker-logo { width:44px;height:44px; } }
'''
(assets / 'workspace.css').write_text(css, encoding='utf-8')
account_path = assets / 'compte' / 'index.html'
account_html = account_path.read_text(encoding='utf-8')
account_html, account_brands = re.subn(r'(<a class="account-brand" href="/essai/">).*?(</a>)', lambda m: m.group(1) + logo_markup + 'AutoDocker' + m.group(2), account_html)
if account_brands != 1:
    raise ValueError('Expected one account brand')
account_html = re.sub(r'<link rel="icon"[^>]*>', f'<link rel="icon" href="{logo_href}" type="image/png">', account_html, count=1)
account_path.write_text(account_html, encoding='utf-8')
shutil.copyfile(source / 'lab-ui' / 'favicon.svg', assets / 'favicon.svg')
tracer = assets / 'tracer'
tracer.mkdir(exist_ok=True)
(tracer / 'compose-viewer.bundle.js').unlink(missing_ok=True)
viewer = tracer_build
if not (viewer / 'browser.html').is_file():
    raise SystemExit('Build the full Docker Tracer browser workspace before assembling trial assets')
shutil.copyfile(viewer / 'browser.html', tracer / 'index.html')
shutil.rmtree(tracer / 'assets', ignore_errors=True)
shutil.copytree(viewer / 'assets', tracer / 'assets')
shutil.copyfile(source / 'LICENSE', tracer / 'assets' / 'license.txt')
print(f'Built trial assets in {assets}')
