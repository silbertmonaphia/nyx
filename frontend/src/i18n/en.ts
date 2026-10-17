import type { zh } from './zh';

type TranslationShape<T> = {
  [K in keyof T]: T[K] extends string ? string : TranslationShape<T[K]>;
};

export type Translation = TranslationShape<typeof zh>;

export const en: Translation = {
  translation: {
    app: {
      title: {
        edit: 'Edit "{{title}}" — Nyx',
        add: 'Add a feed — Nyx',
        signIn: 'Sign in — Nyx',
        chat: 'Chat — Nyx',
        oldestFirst: 'Oldest first — Nyx',
        search: '"{{term}}" — Search — Nyx',
        default: 'Nyx — Your minimalist feed guide',
      },
      description: {
        edit: 'Edit the details of "{{title}}" in your Nyx feed.',
        add: 'Add a new entry to your Nyx feed.',
        signIn: 'Sign in or create a Nyx account to curate your feed.',
        chat: 'Stream a chat with the Nyx assistant.',
        search: 'Search Nyx for "{{term}}".',
        default:
          'A minimalist feed for discovering, tracking, and curating the films you love.',
      },
      authDialog: {
        title: 'Login or Register',
        description:
          'Sign in to your account, or create a new one.',
      },
      deleteDialog: {
        title: 'Delete feed',
        description:
          'This will remove the feed from your list. This action cannot be undone.',
        confirm: 'Delete',
        pending: 'Deleting…',
      },
      logoutAllDialog: {
        title: 'Logout all devices',
        description: 'This revokes your login sessions on every device. This window will be logged out too.',
        confirm: 'Logout all',
        pending: 'Logging out…',
      },
      toast: {
        loginToEdit: 'Please login to edit feeds',
        loginToDelete: 'Please login to delete feeds',
        ownerOnly: 'You can only edit feeds you created',
      },
    },
    nav: {
      searchPlaceholder: 'Search for feeds...',
      chat: 'AI Agent',
      openChat: 'Open AI Agent',
      logout: 'Logout',
      logoutAll: 'Logout all devices',
      loginOrRegister: 'Login / Register',
      addFeed: 'Add feed',
      cancelAddFeed: 'Cancel adding feed',
      sort: {
        newest: 'Newest first',
        oldest: 'Oldest first',
        toggleToOldest: 'Sorted newest first; click to show oldest first',
        toggleToNewest: 'Sorted oldest first; click to show newest first',
      },
    },
    locale: {
      switch: 'Switch language',
    },
    theme: {
      light: 'Switch to light theme',
      dark: 'Switch to dark theme',
    },
    auth: {
      username: 'Username',
      password: 'Password',
      login: 'Login',
      register: 'Register',
      noAccount: "Don't have an account?",
      haveAccount: 'Already have an account?',
      loginSuccess: 'Successfully logged in!',
      registerSuccess: 'Successfully registered!',
      usernameMin: 'Username must be at least 3 characters',
      passwordMin: 'Password must be at least 6 characters',
      rememberMe: 'Remember me',
      invalidCredentials: 'Invalid username or password',
      usernameTaken: 'Username already taken',
      tooManyAttempts: 'Too many attempts. Please try again later.',
      requestFailed: 'Something went wrong. Please try again.',
    },
    feeds: {
      newFeed: 'New Feed',
      editFeed: 'Edit Feed',
      title: 'Title',
      description: 'Description',
      titlePlaceholder: 'Feed title',
      descriptionPlaceholder: 'Description',
      save: 'Save Feed',
      update: 'Update Feed',
      edit: 'Edit',
      delete: 'Delete',
      created: 'Created {{relative}}',
      updated: 'Updated {{relative}}',
      countLoaded_one: '{{count}} feed loaded',
      countLoaded_other: '{{count}} feeds loaded',
      countOfTotal: '{{loaded}} of {{total}} feeds loaded',
      emptySearch: 'Feed "{{term}}" not found',
      empty: 'No feeds found',
      titleRequired: 'Title is required',
      titleTooLong: 'Title is too long',
      descriptionTooLong: 'Description is too long',
    },
    chat: {
      title: 'Chat',
      newChat: 'New chat',
      description:
        'Ask anything — responses stream as the model generates them.',
      emptyState: 'No messages yet — type below to start.',
      placeholder: 'Type a message…',
      send: 'Send',
      busyHint:
        'The agent is still working on this conversation — you can reply when it finishes.',
      showHistory: 'Show chat history',
      hideHistory: 'Hide chat history',
      history: 'History',
      historyAria: 'Chat history',
      loading: 'Loading…',
      noConversations: 'No conversations yet',
      untitledSession: 'New chat',
      stillWorking: 'Agent is still working on this conversation',
      deleteSession: 'Delete {{label}}',
      stepArgs: 'args',
      stepResult: 'result: {{preview}}',
      retryTitle_one: 'succeeded after {{count}} retry',
      retryTitle_other: 'succeeded after {{count}} retries',
      steps_one: '{{count}} step',
      steps_other: '{{count}} steps',
      status: {
        running: 'running',
        ok: 'ok',
        failed: 'failed',
      },
      tokens: 'tokens: {{total}} ( prompt {{prompt}} · completion {{completion}})',
      partialFailurePlan: 'Run did not reach a terminal step',
      partialFailureSteps:
        'Some steps failed — the answer below may be incomplete',
      confirmation: {
        needed: 'Approval needed',
        approved: 'Approved',
        denied: 'Denied',
        expired: 'Expired',
        tool: 'Tool:',
        expiresIn: 'Expires in {{time}}',
        deny: 'Deny',
        approve: 'Approve',
        approvedBody: 'Approved — the agent is running this call.',
        deniedBody: 'Denied — the agent was told the call was rejected.',
        expiredBody:
          'This request expired before a decision — the call was not executed.',
      },
      errors: {
        decision: 'Could not send your decision. Please try again.',
        generic: 'Chat request failed',
        requestFailed: 'Chat request failed ({{status}})',
        invalidJson: 'Chat response was not valid JSON',
        noBody: 'Chat response had no body',
        streamError: 'Chat stream error',
        sessionNoId: 'Chat session creation returned no id',
      },
    },
    time: {
      justNow: 'just now',
      minutesAgo_one: '{{count}} min ago',
      minutesAgo_other: '{{count}} min ago',
      hoursAgo_one: '{{count}} hr ago',
      hoursAgo_other: '{{count}} hr ago',
      daysAgo_one: '{{count}} day ago',
      daysAgo_other: '{{count}} days ago',
      weeksAgo_one: '{{count}} week ago',
      weeksAgo_other: '{{count}} weeks ago',
      monthsAgo_one: '{{count}} month ago',
      monthsAgo_other: '{{count}} months ago',
      yearsAgo_one: '{{count}} year ago',
      yearsAgo_other: '{{count}} years ago',
    },
    errors: {
      unexpected: 'An unexpected error occurred',
      sessionExpired: 'Session expired. Please login again.',
      noResponse: 'No response from server. Please check your connection.',
      serverError: 'Server error ({{status}})',
      serverErrorRef: 'Server error ({{status}}) (ref: {{requestId}})',
      boundaryTitle: 'Something went wrong',
      boundaryBody:
        'An unexpected error stopped the page from rendering. Reload to try again.',
      logoutAllFailed: 'Could not revoke your other sessions. Check your connection and try again.',
    },
    common: {
      cancel: 'Cancel',
      close: 'Close',
    },
  },
};

export default en;
