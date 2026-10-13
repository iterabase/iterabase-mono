import type { LocaleCode } from "./authApi";

export interface AuthCopy {
  brand: string;
  eyebrow: string;
  signInTitle: string;
  signInIntro: string;
  email: string;
  password: string;
  confirmPassword: string;
  passwordMismatch: string;
  signIn: string;
  signingIn: string;
  forgotPassword: string;
  requestAccessLink: string;
  genericSignInError: string;
  sessionExpired: string;
  requestTitle: string;
  requestIntro: string;
  requestSubmit: string;
  requestSubmitting: string;
  requestAccepted: string;
  backToSignIn: string;
  invalidEmail: string;
  forgotTitle: string;
  forgotIntro: string;
  forgotSubmit: string;
  forgotAccepted: string;
  verifyChecking: string;
  verifyWaitingTitle: string;
  verifyWaitingBody: string;
  verifyVerified: string;
  verifyDeclined: string;
  verifyExpired: string;
  verifySuperseded: string;
  verifyInvalid: string;
  verifyUnavailable: string;
  verifyApproved: string;
  setupTitle: string;
  setupIntro: string;
  displayName: string;
  language: string;
  role: string;
  roleOperator: string;
  roleAdmin: string;
  passwordGuidance: string;
  finishSetup: string;
  setupCompleting: string;
  setupDoneTitle: string;
  setupDoneBody: string;
  setupExpired: string;
  setupReused: string;
  setupSuperseded: string;
  setupIneligible: string;
  setupResend: string;
  setupResendSent: string;
  setupRecover: string;
  resetTitle: string;
  resetIntro: string;
  resetGuidance: string;
  resetSubmit: string;
  resetCompleting: string;
  resetDoneTitle: string;
  resetDoneBody: string;
  resetExpired: string;
  resetReused: string;
  resetSuperseded: string;
  resetIneligible: string;
  resetRecover: string;
  passwordTooCommon: string;
  unavailableTitle: string;
  unavailableBody: string;
  signOutFailed: string;
  throttled: string;
  retry: string;
  loading: string;
  signedInAs: string;
  navWork: string;
  navProfile: string;
  navSessions: string;
  navRequests: string;
  signOut: string;
  profileTitle: string;
  profileIntro: string;
  profileSave: string;
  profileSaving: string;
  profileSaved: string;
  profileConflict: string;
  profileEmailReadonly: string;
  profileRoleReadonly: string;
  sessionsTitle: string;
  sessionsIntro: string;
  sessionsLoading: string;
  sessionsError: string;
  currentSession: string;
  regionUnavailable: string;
  clientUnavailable: string;
  activityUnavailable: string;
  lastActive: string;
  created: string;
  signOutThisDevice: string;
  signOutSession: string;
  revokeOthers: string;
  revokeOthersConfirm: string;
  revokeSessionConfirm: string;
  revokeSessionConsequence: string;
  revokeOthersConsequence: string;
  revokeCancel: string;
  revokeConfirm: string;
  revoked: string;
  requestsTitle: string;
  requestsIntro: string;
  requestsLoading: string;
  requestsError: string;
  requestsEmpty: string;
  requestsReview: string;
  requestsDecline: string;
  approveTitle: string;
  approveIntro: string;
  approveAs: string;
  approveSubmit: string;
  declineTitle: string;
  declineIntro: string;
  declineSubmit: string;
  approveDone: string;
  declineDone: string;
  stateChanged: string;
  accountExists: string;
  forbidden: string;
  reauthTitle: string;
  reauthIntro: string;
  reauthSubmit: string;
  reauthCancel: string;
  reauthMismatch: string;
}

const en: AuthCopy = {
  brand: "Iterabase",
  eyebrow: "Account security",
  signInTitle: "Sign in",
  signInIntro: "Use your work email and password to continue.",
  email: "Work email",
  password: "Password",
  confirmPassword: "Confirm password",
  passwordMismatch: "The passwords do not match.",
  signIn: "Sign in",
  signingIn: "Signing in…",
  forgotPassword: "Forgot password?",
  requestAccessLink: "Request access",
  genericSignInError: "We could not sign you in with those details.",
  sessionExpired: "Your session expired. Sign in again to continue.",
  requestTitle: "Request access",
  requestIntro:
    "Access is reviewed by a company Admin. Submitting a request does not grant access.",
  requestSubmit: "Request access",
  requestSubmitting: "Sending…",
  requestAccepted:
    "Check your email to verify this address. Verification does not grant access; an Admin must still review the request.",
  backToSignIn: "Back to sign in",
  invalidEmail: "Enter a valid work email address.",
  forgotTitle: "Reset your password",
  forgotIntro:
    "If an eligible account exists, password-reset instructions will be sent. Your password and active sessions have not changed.",
  forgotSubmit: "Send reset instructions",
  forgotAccepted:
    "If an eligible account exists, password-reset instructions will be sent. Your password and active sessions have not changed.",
  verifyChecking: "Checking this link…",
  verifyWaitingTitle: "Your request is waiting for review",
  verifyWaitingBody:
    "Email verified. Your request is waiting for an Admin review. You cannot sign in yet.",
  verifyVerified: "Email verified.",
  verifyDeclined:
    "This access request was not approved. You can submit a new request if access is still needed.",
  verifyExpired:
    "This request did not reach Admin review. You can submit a new request.",
  verifySuperseded: "A newer verification email was sent. Use the newest link.",
  verifyInvalid: "This link is not valid.",
  verifyUnavailable: "This is temporarily unavailable. Try again shortly.",
  verifyApproved:
    "Access approved. First-time setup is pending — check your email for setup instructions.",
  setupTitle: "Finish setup",
  setupIntro:
    "Choose your password to activate your account. Finishing setup does not sign you in.",
  displayName: "Your name",
  language: "Language",
  role: "Role",
  roleOperator: "Operator",
  roleAdmin: "Admin",
  passwordGuidance:
    "Use 12 to 128 characters. Common passwords are rejected; there are no composition rules.",
  finishSetup: "Finish setup",
  setupCompleting: "Finishing setup…",
  setupDoneTitle: "Setup complete",
  setupDoneBody: "Setup complete. Sign in with your new password to continue.",
  setupExpired: "This setup link expired.",
  setupReused: "This setup link was already used.",
  setupSuperseded: "A newer setup email was sent. Use the newest link.",
  setupIneligible: "This account cannot complete setup.",
  setupResend: "Request new setup instructions",
  setupResendSent:
    "If eligible, new setup instructions will be sent to your email.",
  setupRecover: "Request new setup instructions to continue.",
  resetTitle: "Choose a new password",
  resetIntro:
    "Resetting your password will sign out all browser sessions. API keys are not revoked.",
  resetGuidance:
    "Use 12 to 128 characters. Common passwords are rejected; there are no composition rules.",
  resetSubmit: "Reset password",
  resetCompleting: "Resetting…",
  resetDoneTitle: "Password reset",
  resetDoneBody:
    "Password reset. All browser sessions were signed out. API keys were not revoked. Sign in with your new password.",
  resetExpired: "This reset link expired.",
  resetReused: "This reset link was already used.",
  resetSuperseded: "A newer reset email was sent. Use the newest link.",
  resetIneligible: "This account cannot reset its password.",
  resetRecover: "Request a new password-reset email and try again.",
  passwordTooCommon: "Choose a less common password.",
  unavailableTitle: "Sign-in is temporarily unavailable",
  unavailableBody:
    "Browser authentication is not available in this deployment right now.",
  signOutFailed: "Could not sign out. You are still signed in; try again.",
  throttled: "Too many attempts. Try again shortly.",
  retry: "Try again",
  loading: "Loading…",
  signedInAs: "Signed in as",
  navWork: "Work",
  navProfile: "Profile",
  navSessions: "Sessions",
  navRequests: "Access requests",
  signOut: "Sign out this device",
  profileTitle: "Profile",
  profileIntro: "Update your display name and language.",
  profileSave: "Save changes",
  profileSaving: "Saving…",
  profileSaved: "Changes saved.",
  profileConflict:
    "Your profile changed elsewhere. Reload to see the current values.",
  profileEmailReadonly: "Work email",
  profileRoleReadonly: "Role",
  sessionsTitle: "Browser sessions",
  sessionsIntro: "Only your active sessions are shown.",
  sessionsLoading: "Loading sessions…",
  sessionsError: "Sessions could not be loaded.",
  currentSession: "Current session",
  regionUnavailable: "Region unavailable",
  clientUnavailable: "Client unavailable",
  activityUnavailable: "Activity unavailable",
  lastActive: "Last activity",
  created: "Signed in",
  signOutThisDevice: "Sign out this device",
  signOutSession: "Sign out",
  revokeOthers: "Sign out all other sessions",
  revokeOthersConfirm: "Sign out all other sessions?",
  revokeSessionConfirm: "Sign out this browser session?",
  revokeSessionConsequence:
    "This browser session will be signed out. Its next authenticated request will be denied. Work already authorized is not reversed.",
  revokeOthersConsequence:
    "Sign out all other browser sessions. This session and your API keys are unchanged.",
  revokeCancel: "Cancel",
  revokeConfirm: "Sign out",
  revoked: "Session signed out.",
  requestsTitle: "Access requests",
  requestsIntro: "Verified requests waiting for a decision.",
  requestsLoading: "Loading requests…",
  requestsError: "Requests could not be loaded.",
  requestsEmpty: "No verified requests are waiting for review.",
  requestsReview: "Review",
  requestsDecline: "Decline",
  approveTitle: "Approve access",
  approveIntro:
    "This creates a setup-pending person and sends first-time setup instructions. They cannot sign in until setup is complete.",
  approveAs: "Approve as",
  approveSubmit: "Approve",
  declineTitle: "Decline this request?",
  declineIntro:
    "No person will be created. A new access request can be submitted later.",
  declineSubmit: "Decline request",
  approveDone: "Access approved. First-time setup is pending.",
  declineDone: "Request declined. A new request can be submitted later.",
  stateChanged: "This request changed. Refresh to see the current state.",
  accountExists: "An account already exists for this email.",
  forbidden: "This area is not available with your current access.",
  reauthTitle: "Confirm it is you",
  reauthIntro:
    "Enter your current password to continue. This confirms the action in this browser session; it does not create another session.",
  reauthSubmit: "Confirm",
  reauthCancel: "Cancel",
  reauthMismatch: "We could not confirm your password.",
};

const pt: AuthCopy = {
  brand: "Iterabase",
  eyebrow: "Segurança da conta",
  signInTitle: "Iniciar sessão",
  signInIntro: "Use o seu email de trabalho e palavra-passe para continuar.",
  email: "Email de trabalho",
  password: "Palavra-passe",
  confirmPassword: "Confirmar palavra-passe",
  passwordMismatch: "As palavras-passe não coincidem.",
  signIn: "Iniciar sessão",
  signingIn: "A iniciar sessão…",
  forgotPassword: "Esqueceu-se da palavra-passe?",
  requestAccessLink: "Pedir acesso",
  genericSignInError: "Não foi possível iniciar sessão com estes dados.",
  sessionExpired:
    "A sua sessão expirou. Inicie sessão novamente para continuar.",
  requestTitle: "Pedir acesso",
  requestIntro:
    "O acesso é analisado por um Administrador. Enviar o pedido não concede acesso.",
  requestSubmit: "Pedir acesso",
  requestSubmitting: "A enviar…",
  requestAccepted:
    "Consulte o seu email para confirmar este endereço. A confirmação não concede acesso; um Administrador ainda tem de analisar o pedido.",
  backToSignIn: "Voltar a iniciar sessão",
  invalidEmail: "Introduza um email de trabalho válido.",
  forgotTitle: "Repor a palavra-passe",
  forgotIntro:
    "Se existir uma conta elegível, serão enviadas instruções para repor a palavra-passe. A palavra-passe e as sessões ativas não foram alteradas.",
  forgotSubmit: "Enviar instruções",
  forgotAccepted:
    "Se existir uma conta elegível, serão enviadas instruções para repor a palavra-passe. A palavra-passe e as sessões ativas não foram alteradas.",
  verifyChecking: "A verificar este link…",
  verifyWaitingTitle: "O seu pedido aguarda análise",
  verifyWaitingBody:
    "Email confirmado. O seu pedido aguarda a análise de um Administrador. Ainda não pode iniciar sessão.",
  verifyVerified: "Email confirmado.",
  verifyDeclined:
    "Este pedido de acesso não foi aprovado. Pode enviar um novo pedido se ainda precisar de acesso.",
  verifyExpired:
    "Este pedido não chegou à análise de um Administrador. Pode enviar um novo pedido.",
  verifySuperseded:
    "Foi enviado um email de confirmação mais recente. Utilize o link mais recente.",
  verifyInvalid: "Este link não é válido.",
  verifyUnavailable: "Isto está temporariamente indisponível. Tente novamente.",
  verifyApproved:
    "Acesso aprovado. A primeira configuração está pendente — consulte o email.",
  setupTitle: "Concluir configuração",
  setupIntro:
    "Escolha a sua palavra-passe para ativar a conta. Concluir a configuração não inicia sessão.",
  displayName: "O seu nome",
  language: "Idioma",
  role: "Função",
  roleOperator: "Operador",
  roleAdmin: "Administrador",
  passwordGuidance:
    "Use entre 12 e 128 caracteres. Palavras-passe comuns são recusadas; não há regras de composição.",
  finishSetup: "Concluir configuração",
  setupCompleting: "A concluir…",
  setupDoneTitle: "Configuração concluída",
  setupDoneBody:
    "Configuração concluída. Inicie sessão com a nova palavra-passe para continuar.",
  setupExpired: "Este link de configuração expirou.",
  setupReused: "Este link de configuração já foi utilizado.",
  setupSuperseded:
    "Foi enviado um email de configuração mais recente. Utilize o link mais recente.",
  setupIneligible: "Esta conta não pode concluir a configuração.",
  setupResend: "Pedir novas instruções",
  setupResendSent:
    "Se elegível, serão enviadas novas instruções para o seu email.",
  setupRecover: "Peça novas instruções de configuração para continuar.",
  resetTitle: "Escolher nova palavra-passe",
  resetIntro:
    "Repor a palavra-passe termina todas as sessões do browser. As chaves de API não são revogadas.",
  resetGuidance:
    "Use entre 12 e 128 caracteres. Palavras-passe comuns são recusadas; não há regras de composição.",
  resetSubmit: "Repor palavra-passe",
  resetCompleting: "A repor…",
  resetDoneTitle: "Palavra-passe reposta",
  resetDoneBody:
    "Palavra-passe reposta. Todas as sessões do browser foram terminadas. As chaves de API não foram revogadas. Inicie sessão com a nova palavra-passe.",
  resetExpired: "Este link de reposição expirou.",
  resetReused: "Este link de reposição já foi utilizado.",
  resetSuperseded:
    "Foi enviado um email de reposição mais recente. Utilize o link mais recente.",
  resetIneligible: "Esta conta não pode repor a palavra-passe.",
  resetRecover: "Peça um novo email de reposição e tente novamente.",
  passwordTooCommon: "Escolha uma palavra-passe menos comum.",
  unavailableTitle: "Início de sessão temporariamente indisponível",
  unavailableBody:
    "A autenticação do browser não está disponível nesta instalação neste momento.",
  signOutFailed:
    "Não foi possível terminar a sessão. Continua com sessão iniciada; tente novamente.",
  throttled: "Demasiadas tentativas. Tente novamente mais tarde.",
  retry: "Tentar novamente",
  loading: "A carregar…",
  signedInAs: "Sessão iniciada como",
  navWork: "Trabalho",
  navProfile: "Perfil",
  navSessions: "Sessões",
  navRequests: "Pedidos de acesso",
  signOut: "Terminar esta sessão",
  profileTitle: "Perfil",
  profileIntro: "Atualize o seu nome e idioma.",
  profileSave: "Guardar alterações",
  profileSaving: "A guardar…",
  profileSaved: "Alterações guardadas.",
  profileConflict:
    "O seu perfil foi alterado noutro local. Recarregue para ver os valores atuais.",
  profileEmailReadonly: "Email de trabalho",
  profileRoleReadonly: "Função",
  sessionsTitle: "Sessões do browser",
  sessionsIntro: "Apenas as suas sessões ativas são apresentadas.",
  sessionsLoading: "A carregar sessões…",
  sessionsError: "Não foi possível carregar as sessões.",
  currentSession: "Sessão atual",
  regionUnavailable: "Região indisponível",
  clientUnavailable: "Cliente indisponível",
  activityUnavailable: "Atividade indisponível",
  lastActive: "Última atividade",
  created: "Início",
  signOutThisDevice: "Terminar esta sessão",
  signOutSession: "Terminar",
  revokeOthers: "Terminar todas as outras sessões",
  revokeOthersConfirm: "Terminar todas as outras sessões?",
  revokeSessionConfirm: "Terminar esta sessão do browser?",
  revokeSessionConsequence:
    "Esta sessão do browser será terminada. O pedido autenticado seguinte será recusado. O trabalho já autorizado não é revertido.",
  revokeOthersConsequence:
    "Terminar todas as outras sessões do browser. Esta sessão e as suas chaves de API não são alteradas.",
  revokeCancel: "Cancelar",
  revokeConfirm: "Terminar",
  revoked: "Sessão terminada.",
  requestsTitle: "Pedidos de acesso",
  requestsIntro: "Pedidos confirmados à espera de decisão.",
  requestsLoading: "A carregar pedidos…",
  requestsError: "Não foi possível carregar os pedidos.",
  requestsEmpty: "Não há pedidos confirmados à espera de análise.",
  requestsReview: "Analisar",
  requestsDecline: "Recusar",
  approveTitle: "Aprovar acesso",
  approveIntro:
    "Isto cria uma pessoa com configuração pendente e envia as instruções de primeira configuração. Não poderá iniciar sessão até concluir a configuração.",
  approveAs: "Aprovar como",
  approveSubmit: "Aprovar",
  declineTitle: "Recusar este pedido?",
  declineIntro:
    "Não será criada nenhuma pessoa. Poderá ser enviado um novo pedido mais tarde.",
  declineSubmit: "Recusar pedido",
  approveDone: "Acesso aprovado. A primeira configuração está pendente.",
  declineDone: "Pedido recusado. Poderá ser enviado um novo pedido.",
  stateChanged: "Este pedido mudou. Atualize para ver o estado atual.",
  accountExists: "Já existe uma conta para este email.",
  forbidden: "Esta área não está disponível com o seu acesso atual.",
  reauthTitle: "Confirme que é você",
  reauthIntro:
    "Introduza a sua palavra-passe atual para continuar. Isto confirma a ação nesta sessão do browser; não cria outra sessão.",
  reauthSubmit: "Confirmar",
  reauthCancel: "Cancelar",
  reauthMismatch: "Não foi possível confirmar a palavra-passe.",
};

export function authCopy(locale: LocaleCode): AuthCopy {
  return locale === "pt" ? pt : en;
}
