import React, { useState } from 'react';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { z } from 'zod';
import api from '../../../services/api';
import { useAuthStore } from '../../../store/authStore';
import { useUiStore } from '../../../store/uiStore';

const authSchema = z.object({
  username: z.string().min(3, 'Username must be at least 3 characters'),
  email: z.string().email('Invalid email address').optional().or(z.literal('')),
  password: z.string().min(6, 'Password must be at least 6 characters'),
});

type AuthFormData = z.infer<typeof authSchema>;

interface AuthFormProps {
  onSuccess: () => void;
  onCancel: () => void;
}

export const AuthForm: React.FC<AuthFormProps> = ({ onSuccess, onCancel }) => {
  const [isLogin, setIsLogin] = useState(true);
  const setAuth = useAuthStore((state) => state.setAuth);
  const addToast = useUiStore((state) => state.addToast);

  const {
    register,
    handleSubmit,
    formState: { errors },
  } = useForm<AuthFormData>({
    resolver: zodResolver(authSchema),
    defaultValues: {
      username: '',
      email: '',
      password: '',
    },
  });

  const onSubmit = async (data: AuthFormData) => {
    try {
      const endpoint = isLogin ? '/login' : '/register';
      const response = await api.post(endpoint, data);
      setAuth(response.data.user, response.data.token);
      addToast(isLogin ? 'Successfully logged in!' : 'Successfully registered!', 'success');
      onSuccess();
    } catch (err) {
      console.error('Auth error:', err);
    }
  };

  return (
    <div className="auth-form-container">
      <form className="add-movie-form auth-form" onSubmit={handleSubmit(onSubmit)}>
        <h2>{isLogin ? 'Login' : 'Register'}</h2>
        
        <div className="form-field">
          <input
            {...register('username')}
            type="text"
            placeholder="Username"
            aria-invalid={!!errors.username}
          />
          {errors.username && <span className="error-message">{errors.username.message}</span>}
        </div>

        {!isLogin && (
          <div className="form-field">
            <input
              {...register('email')}
              type="email"
              placeholder="Email"
              aria-invalid={!!errors.email}
            />
            {errors.email && <span className="error-message">{errors.email.message}</span>}
          </div>
        )}

        <div className="form-field">
          <input
            {...register('password')}
            type="password"
            placeholder="Password"
            aria-invalid={!!errors.password}
          />
          {errors.password && <span className="error-message">{errors.password.message}</span>}
        </div>

        <div className="form-actions">
          <button type="submit" className="submit-button">
            {isLogin ? 'Login' : 'Register'}
          </button>
          <button type="button" className="cancel-button" onClick={onCancel}>
            Cancel
          </button>
        </div>

        <p className="auth-toggle">
          {isLogin ? "Don't have an account? " : 'Already have an account? '}
          <button
            type="button"
            className="toggle-button"
            onClick={() => setIsLogin(!isLogin)}
          >
            {isLogin ? 'Register' : 'Login'}
          </button>
        </p>
      </form>
    </div>
  );
};
