'use strict';
(() => {
  const $=id=>document.getElementById(id);
  const management=document.body.dataset.management==='true';
  const token=document.querySelector('meta[name="csrf-token"]')?.content || '';
  let savedLocalSetup={provider:'',base_url:'',model:'',api_provider:'gemini',docker_host:'auto',has_api_key:false};
  let theme='light';try{if(localStorage.getItem('autodocker:theme')==='dark')theme='dark';}catch{}
  const applyTheme=()=>{document.documentElement.dataset.theme=theme;$('account-theme').textContent=theme==='dark'?'Mode clair':'Mode sombre';};
  applyTheme();$('account-theme').addEventListener('click',()=>{theme=theme==='dark'?'light':'dark';applyTheme();try{localStorage.setItem('autodocker:theme',theme);}catch{}});

  async function api(path,body){
    const response=await fetch(path,{method:body===undefined?'GET':'POST',credentials:'same-origin',cache:'no-store',headers:{'Content-Type':'application/json','X-CSRFToken':token},...(body===undefined?{}:{body:JSON.stringify(body)})});
    if(response.status===401){location.assign('/login');throw new Error('Reconnectez-vous pour continuer.');}
    const data=await response.json();if(!response.ok)throw new Error(data.error || 'Cette action n’a pas pu être effectuée.');return data;
  }
  function status(form,text,error=false){const field=form.querySelector('.account-form-status');field.textContent=text;field.classList.toggle('error',error);field.hidden=!text;}
  async function submit(form,action){
    const buttons=[...form.querySelectorAll('button')];buttons.forEach(button=>button.disabled=true);form.setAttribute('aria-busy','true');status(form,'Enregistrement…');
    try{await action();}catch(error){status(form,error.message,true);}finally{buttons.forEach(button=>button.disabled=false);form.removeAttribute('aria-busy');}
  }
  function element(tag,text,className){const node=document.createElement(tag);if(text!==undefined)node.textContent=text;if(className)node.className=className;return node;}
  function updateLocalFields(){
    const provider=$('local-ai-provider').value;
    $('local-ai-url-field').hidden=!['ollama','vllm','lmstudio'].includes(provider);
    $('local-ai-api-field').hidden=provider!=='api';
    $('local-ai-model-field').hidden=provider==='';
    $('local-ai-key-field').hidden=provider!=='api';
    $('local-ai-url').placeholder=provider==='ollama'?'http://127.0.0.1:11434':provider==='lmstudio'?'http://127.0.0.1:1234':'http://127.0.0.1:8000';
    $('local-ai-model-help').textContent=provider==='ollama'?'Vide : modèle local par défaut.':provider==='api'?'Nom exact du modèle chez le fournisseur.':'Nom exact du modèle déjà chargé.';
    $('local-ai-key-remove-field').hidden=!savedLocalSetup.has_api_key;
    $('local-docker-host-field').hidden=$('local-docker-mode').value!=='manual';
  }
  function localSetupInput(){
    return {provider:$('local-ai-provider').value,base_url:$('local-ai-url').value.trim(),model:$('local-ai-model').value.trim(),api_provider:$('local-ai-api-provider').value,docker_host:$('local-docker-mode').value==='manual'?$('local-docker-host').value.trim():'auto',api_key:$('local-ai-key').value.trim(),remove_api_key:$('local-ai-key-remove').checked};
  }
  async function loadDockerStatus(){
    try{const data=await api('/api/account/setup/docker');$('local-docker-status').textContent=data.message;$('local-docker-status').classList.toggle('available',!!data.available);}
    catch(error){$('local-docker-status').textContent=error.message;$('local-docker-status').classList.remove('available');}
  }
  async function loadLocalSetup(){
    savedLocalSetup=await api('/api/account/setup');
    $('local-ai-provider').value=savedLocalSetup.provider||'';
    $('local-ai-url').value=savedLocalSetup.base_url||'';
    $('local-ai-model').value=savedLocalSetup.model||'';
    $('local-ai-api-provider').value=savedLocalSetup.api_provider||'gemini';
    $('local-docker-mode').value=savedLocalSetup.docker_host&&savedLocalSetup.docker_host!=='auto'?'manual':'auto';
    $('local-docker-host').value=savedLocalSetup.docker_host&&savedLocalSetup.docker_host!=='auto'?savedLocalSetup.docker_host:(navigator.platform.includes('Win')?'npipe:////./pipe/docker_engine':'unix:///var/run/docker.sock');
    $('local-ai-key-hint').textContent=savedLocalSetup.has_api_key?'Une clé est enregistrée. Laissez vide pour la garder.':'La clé ne sera plus affichée après enregistrement.';
    updateLocalFields();
    const mfa=await api('/api/totp/status');
    $('local-mfa').hidden=!mfa.setup_required;
    $('local-config-controls').hidden=!!mfa.setup_required;
    $('owner-setup').hidden=false;
    if(!mfa.setup_required)void loadDockerStatus();
  }
  async function loadUsers(){
    const data=await api('/api/accounts');$('users-list').replaceChildren();$('users-empty').hidden=data.users.length>0;$('users-count').textContent=data.users.length+' compte'+(data.users.length===1?'':'s');
    for(const user of data.users){
      const row=element('tr');const identity=element('td');identity.append(element('strong',user.username));if(user.owner)identity.append(element('span','Propriétaire','account-owner-label'));
      const email=element('td',user.email||'Non renseignée');const state=element('td');state.append(element('span',user.disabled?'Désactivé':'Actif','account-state'+(user.disabled?' disabled':'')));const actions=element('td');
      row.append(identity,email,state,actions);$('users-list').append(row);
      if(user.owner){actions.append(element('a','Mon compte'));actions.firstChild.href='/compte';continue;}
      const controls=element('div',undefined,'account-actions');const toggle=element('button',user.disabled?'Réactiver':'Désactiver');toggle.type='button';toggle.setAttribute('aria-label',(user.disabled?'Réactiver ':'Désactiver ')+user.username);
      toggle.addEventListener('click',async()=>{toggle.disabled=true;try{await api('/api/accounts/'+encodeURIComponent(user.username)+'/status',{disabled:!user.disabled});await loadUsers();}catch(error){$('account-global-error').textContent=error.message;$('account-global-error').hidden=false;toggle.disabled=false;}});
      const reset=element('button','Changer le mot de passe');reset.type='button';reset.setAttribute('aria-label','Changer le mot de passe de '+user.username);reset.setAttribute('aria-expanded','false');controls.append(toggle,reset);actions.append(controls);
      const extra=element('tr');extra.hidden=true;const cell=element('td',undefined,'account-reset-cell');cell.colSpan=4;const form=element('form',undefined,'account-reset');form.method='post';const field=element('div',undefined,'account-field');const label=element('label','Nouveau mot de passe pour '+user.username);const input=element('input');input.id='reset-'+user.username;label.htmlFor=input.id;input.type='password';input.autocomplete='new-password';input.required=true;input.minLength=8;input.maxLength=72;field.append(label,input);const save=element('button','Enregistrer','account-primary');save.type='submit';const cancel=element('button','Annuler','account-cancel');cancel.type='button';const message=element('p',undefined,'account-form-status');message.setAttribute('role','status');message.hidden=true;form.append(field,save,cancel,message);cell.append(form);extra.append(cell);$('users-list').append(extra);
      reset.addEventListener('click',()=>{extra.hidden=!extra.hidden;reset.setAttribute('aria-expanded',String(!extra.hidden));if(!extra.hidden)input.focus();});cancel.addEventListener('click',()=>{extra.hidden=true;input.value='';reset.setAttribute('aria-expanded','false');reset.focus();});
      form.addEventListener('submit',event=>{event.preventDefault();void submit(form,async()=>{await api('/api/accounts/'+encodeURIComponent(user.username)+'/password',{password:input.value});input.value='';status(form,'Mot de passe modifié. Les anciennes sessions ont été fermées.');});});
    }
  }
  async function load(){
    try{
      const account=await api('/api/account');$('users-navigation').hidden=!account.manage_users;
      if(management){if(!account.manage_users)throw new Error('Cette page est réservée au propriétaire.');await loadUsers();}
      else{$('account-username').textContent=account.username;$('account-email').value=account.email;if(account.manage_users)await loadLocalSetup();}
      $('account-content').hidden=false;
    }catch(error){$('account-global-error').textContent=error.message;$('account-global-error').hidden=false;}finally{$('account-loading').hidden=true;}
  }
  if(management){
    $('create-account').addEventListener('submit',event=>{event.preventDefault();const form=event.currentTarget;void submit(form,async()=>{const created=await api('/api/accounts',{username:$('new-username').value.trim(),password:$('initial-password').value});form.reset();await loadUsers();status(form,'Le compte '+created.username+' est créé.');});});
  }else{
    $('local-mfa-start').addEventListener('click',async()=>{const button=$('local-mfa-start');button.disabled=true;const note=$('local-mfa-message');note.hidden=false;note.textContent='Préparation du code…';try{const data=await api('/api/totp/setup',{});$('local-mfa-qr').src='data:image/png;base64,'+data.qr;$('local-mfa-manual').textContent=data.secret;$('local-mfa-secret').hidden=false;note.hidden=true;}catch(error){note.textContent=error.message;button.disabled=false;}});
    $('local-mfa-verify').addEventListener('click',async()=>{const note=$('local-mfa-message');note.hidden=false;note.textContent='Vérification…';try{await api('/api/totp/verify',{code:$('local-mfa-code').value.trim()});$('local-mfa-secret').hidden=true;$('local-mfa').hidden=true;$('local-config-controls').hidden=false;$('local-mfa-manual').textContent='';$('local-mfa-qr').removeAttribute('src');void loadDockerStatus();}catch(error){note.textContent=error.message;}});
    $('local-ai-provider').addEventListener('change',updateLocalFields);
    $('local-docker-mode').addEventListener('change',updateLocalFields);
    $('local-setup-form').addEventListener('submit',event=>{event.preventDefault();const form=event.currentTarget;void submit(form,async()=>{const data=await api('/api/account/setup',localSetupInput());savedLocalSetup=data;$('local-ai-key').value='';$('local-ai-key-remove').checked=false;updateLocalFields();status(form,'Réglages enregistrés.');});});
    $('local-ai-test').addEventListener('click',()=>{const form=$('local-setup-form');void submit(form,async()=>{const result=await api('/api/account/setup/test-ai',localSetupInput());status(form,result.message,!result.available);});});
    $('local-docker-search').addEventListener('click',()=>{void loadDockerStatus();});
    $('local-docker-connect').addEventListener('click',async()=>{const statusNode=$('local-docker-message');statusNode.hidden=false;statusNode.textContent='Connexion en cours…';try{const dockerHost=$('local-docker-mode').value==='manual'?$('local-docker-host').value.trim():'auto';const data=await api('/api/account/setup',{...savedLocalSetup,docker_host:dockerHost,api_key:'',remove_api_key:false});savedLocalSetup=data;await loadDockerStatus();statusNode.textContent='Choix Docker enregistré.';}catch(error){statusNode.textContent=error.message;statusNode.classList.add('error');}});
    $('update-email').addEventListener('submit',event=>{event.preventDefault();const form=event.currentTarget;void submit(form,async()=>{const data=await api('/api/account/email',{email:$('account-email').value.trim()});status(form,data.message);});});
    $('update-password').addEventListener('submit',event=>{event.preventDefault();const form=event.currentTarget;void submit(form,async()=>{const data=await api('/api/account/password',{current_password:$('current-password').value,new_password:$('new-password').value});form.reset();status(form,data.message);});});
  }
  void load();
})();
