import axios from 'axios';
import { useUiStore } from '../store/uiStore';

const api = axios.create({
  baseURL: import.meta.env.VITE_API_URL || 'http://localhost:8080/api',
  headers: {
    'Content-Type': 'application/json',
  },
});

// Response interceptor for global error handling
api.interceptors.response.use(
  (response) => {
    // Show success toasts for non-GET requests if needed
    if (response.config.method !== 'get' && response.status >= 200 && response.status < 300) {
        // Optionally show success messages here
    }
    return response;
  },
  (error) => {
    let message = 'An unexpected error occurred';
    
    if (error.response) {
      // The request was made and the server responded with a status code
      // that falls out of the range of 2xx
      const backendError = error.response.data;
      if (backendError && backendError.error) {
        message = backendError.error;
      } else {
        message = `Server error: ${error.response.status}`;
      }
    } else if (error.request) {
      // The request was made but no response was received
      message = 'No response from server. Please check your connection.';
    } else {
      // Something happened in setting up the request that triggered an Error
      message = error.message;
    }

    useUiStore.getState().addToast(message, 'error');
    
    return Promise.reject(error);
  }
);

export default api;
