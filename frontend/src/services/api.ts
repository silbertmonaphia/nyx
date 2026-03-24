import axios from 'axios';

const api = axios.create({
  baseURL: import.meta.env.VITE_API_URL || 'http://localhost:8080/api',
  headers: {
    'Content-Type': 'application/json',
  },
});

// Optional: Add request interceptors here for things like auth tokens
// api.interceptors.request.use(
//   (config) => {
//     const token = localStorage.getItem('authToken'); // Example
//     if (token) {
//       config.headers.Authorization = `Bearer ${token}`;
//     }
//     return config;
//   },
//   (error) => Promise.reject(error)
// );

// Optional: Add response interceptors here for global error handling
// api.interceptors.response.use(
//   (response) => response,
//   (error) => {
//     if (error.response && error.response.status === 401) {
//       // Handle unauthorized errors, e.g., redirect to login
//       console.error('Unauthorized, redirecting to login...');
//     }
//     return Promise.reject(error);
//   }
// );

export default api;
