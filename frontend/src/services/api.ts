import axios from 'axios';
import { useUiStore } from '../store/uiStore';
import { useAuthStore } from '../store/authStore';

const api = axios.create({
  baseURL: import.meta.env.VITE_API_URL || 'http://localhost:8080/api',
  headers: {
    'Content-Type': 'application/json',
  },
});

// Request interceptor for adding auth token
api.interceptors.request.use(
  (config) => {
    const token = useAuthStore.getState().token;
    if (token) {
      config.headers.Authorization = `Bearer ${token}`;
    }
    return config;
  },
  (error) => Promise.reject(error)
);

// Response interceptor for global error handling
api.interceptors.response.use(
  (response) => {
    return response;
  },
  (error) => {
    let message = 'An unexpected error occurred';
    
    if (error.response) {
      // Handle 401 Unauthorized
      if (error.response.status === 401) {
        useAuthStore.getState().logout();
        message = 'Session expired. Please login again.';
      } else {
        const backendError = error.response.data;
        if (backendError && backendError.error) {
          message = backendError.error;
        } else {
          message = `Server error: ${error.response.status}`;
        }
      }
    } else if (error.request) {
      message = 'No response from server. Please check your connection.';
    } else {
      message = error.message;
    }

    useUiStore.getState().addToast(message, 'error');
    
    return Promise.reject(error);
  }
);

export default api;
