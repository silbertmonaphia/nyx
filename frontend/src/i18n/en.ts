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
      llmConfig: 'LLM settings',
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
      tokensTotalOnly: 'tokens: {{total}}',
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
    llmConfig: {
      title: 'LLM configuration',
      description:
        'Bring your own keys — endpoints, models and API keys for chat, search and retrieval.',
      gate: {
        title: 'LLM configuration required',
        description:
          'The AI Agent needs your own model endpoint and API key to run. Fill in the three main-LLM fields to continue — Embedding and Tavily are optional.',
      },
      sections: {
        llm: {
          title: 'Main LLM',
          drives: "Drives chat replies and the agent's planning steps.",
          capabilities:
            'AI Agent needs three provider capabilities: streaming chat completions, OpenAI-shaped tool calls, and structured (json_schema) output. Without tool calls the AI Agent is unavailable and only the single-turn chat path works; without structured output the planner falls back to its legacy free-text path.',
          credentials:
            'Get a key from the DeepSeek console (platform.deepseek.com) or OpenRouter (openrouter.ai), or run a local vLLM server and point the base URL at it.',
          docsLabel: 'DeepSeek API documentation',
          urlConvention:
            'Must be an OpenAI-compatible endpoint: the path carries the version prefix (a trailing /v1), e.g. https://api.deepseek.com/v1.',
          modelNote:
            'The model must exist on the endpoint (it is not probed when saving). An endpoint that needs no credential still requires a non-placeholder value.',
        },
        embedding: {
          title: 'Embedding (optional)',
          drives: 'Drives feed semantic search and RAG retrieval.',
          impact:
            'Optional — leave this empty and the AI Agent cannot retrieve your feeds (RAG / semantic search). Chat, planning and tools keep working.',
          expand: 'Show embedding fields',
          collapse: 'Hide embedding fields',
          separateProvider:
            'Must expose OpenAI-compatible embeddings — not every chat provider offers them, so this slot may point at a different service than the main LLM.',
          credentials:
            'Get a key from the SiliconFlow console (cloud.siliconflow.cn), or run a local vLLM embedding server (e.g. Qwen/Qwen3-Embedding-0.6B via make dev-vllm-embed).',
          docsLabel: 'SiliconFlow API documentation',
          urlConvention:
            'Must be an OpenAI-compatible endpoint: the path carries the version prefix (a trailing /v1), e.g. http://localhost:8000/v1.',
          dimensionNote:
            'The deployment requires 1024-dimension vectors, e.g. Qwen/Qwen3-Embedding-0.6B. A model emitting a different width is rejected at query time, not when saving.',
        },
        tavily: {
          title: 'Tavily search (optional)',
          drives: "Drives the agent's web_search tool.",
          impact:
            'Optional — leave this empty and the AI Agent will not plan web searches. Chat, planning and retrieval keep working.',
          expand: 'Show Tavily fields',
          collapse: 'Hide Tavily fields',
          credentials:
            'Create a key in the Tavily console (app.tavily.com) — the free tier covers light use.',
          docsLabel: 'Tavily search API documentation',
          shapeNote:
            "This slot is shaped for the deployment's search provider (Tavily).",
        },
        fallback: {
          title: 'Retry model (optional)',
          drives:
            'When left empty this only affects the single-turn chat fallback path, never agent runs; when set, it takes over if the main LLM errors.',
          docsLabel: 'Provider API documentation',
          expand: 'Show retry model fields',
          collapse: 'Hide retry model fields',
        },
        agentNote:
          'Only the three main-LLM fields are required. API keys may be left empty for localhost / 127.0.0.1 endpoints. Embedding is optional: leave it empty and the agent cannot retrieve your feeds. Tavily is optional: leave it empty and the agent will not plan web searches.',
      },
      fields: {
        baseUrl: 'Base URL',
        model: 'Model',
        apiKey: 'API key',
        keyPlaceholder: 'Your API key',
        keySaved: 'Saved {{masked}} — leave blank to keep',
      },
      errors: {
        required: 'Required',
        url: 'Enter a valid http(s) URL',
      },
      actions: {
        save: 'Save',
        saving: 'Saving…',
        clear: 'Clear configuration',
        clearing: 'Clearing…',
      },
      confirm: {
        title: 'Clear LLM configuration?',
        description:
          'Your stored endpoints and keys will be deleted. The AI Agent stays unavailable until you configure it again.',
        confirm: 'Clear',
      },
      toasts: {
        saved: 'LLM configuration saved',
        cleared: 'LLM configuration cleared',
        failed: 'Could not update your LLM configuration. Please try again.',
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
