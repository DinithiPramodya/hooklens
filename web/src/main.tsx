import { StrictMode } from 'react'
import { createRoot } from 'react-dom/client'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import './index.css'
import App from './App.tsx'

// One client for the whole app. Created outside the component tree so a
// re-render cannot replace it and throw the cache away.
const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      // Two failed attempts, not three. This is a local debugging tool: a
      // server that is down is usually down because the developer stopped it,
      // and a long retry chain just delays the error that tells them so.
      retry: 1,
    },
  },
})

createRoot(document.getElementById('root')!).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <App />
    </QueryClientProvider>
  </StrictMode>,
)
