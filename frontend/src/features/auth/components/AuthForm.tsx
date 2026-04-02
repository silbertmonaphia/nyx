import React, { useState } from 'react';
import { useForm } from 'react-hook-form';
import { zodResolver } from '@hookform/resolvers/zod';
import { z } from 'zod';
import api from '../../../services/api';
import { useAuthStore } from '../../../store/authStore';
import { useUiStore } from '../../../store/uiStore';
import { Button } from '~/components/ui/Button';
import { Input } from '~/components/ui/Input';
import { Label } from '~/components/ui/Label';
import { Card, CardHeader, CardTitle, CardContent, CardFooter } from '~/components/ui/Card';

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
    <Card className="w-full max-w-[400px] mx-auto my-8">
      <CardHeader>
        <CardTitle className="text-2xl text-center">{isLogin ? 'Login' : 'Register'}</CardTitle>
      </CardHeader>
      <form onSubmit={handleSubmit(onSubmit)}>
        <CardContent className="space-y-4">
          <div className="space-y-2 text-left">
            <Label htmlFor="username">Username</Label>
            <Input
              id="username"
              {...register('username')}
              placeholder="Your username"
              aria-invalid={!!errors.username}
              className={errors.username ? "border-destructive" : ""}
            />
            {errors.username && <p className="text-xs font-medium text-destructive">{errors.username.message}</p>}
          </div>

          {!isLogin && (
            <div className="space-y-2 text-left">
              <Label htmlFor="email">Email</Label>
              <Input
                id="email"
                type="email"
                {...register('email')}
                placeholder="Your email address"
                aria-invalid={!!errors.email}
                className={errors.email ? "border-destructive" : ""}
              />
              {errors.email && <p className="text-xs font-medium text-destructive">{errors.email.message}</p>}
            </div>
          )}

          <div className="space-y-2 text-left">
            <Label htmlFor="password">Password</Label>
            <Input
              id="password"
              type="password"
              {...register('password')}
              placeholder="••••••••"
              aria-invalid={!!errors.password}
              className={errors.password ? "border-destructive" : ""}
            />
            {errors.password && <p className="text-xs font-medium text-destructive">{errors.password.message}</p>}
          </div>
        </CardContent>
        <CardFooter className="flex flex-col gap-4">
          <div className="flex gap-2 w-full">
            <Button type="submit" className="flex-1">
              {isLogin ? 'Login' : 'Register'}
            </Button>
            <Button type="button" variant="outline" onClick={onCancel} className="flex-1">
              Cancel
            </Button>
          </div>
          <p className="text-sm text-center text-muted-foreground">
            {isLogin ? "Don't have an account? " : 'Already have an account? '}
            <button
              type="button"
              className="text-primary font-semibold hover:underline underline-offset-4"
              onClick={() => setIsLogin(!isLogin)}
            >
              {isLogin ? 'Register' : 'Login'}
            </button>
          </p>
        </CardFooter>
      </form>
    </Card>
  );
};
