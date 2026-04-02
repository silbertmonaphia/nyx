import { test, expect } from '@playwright/test';

test.describe('Authentication and Movie CRUD', () => {
  const testUser = {
    username: `user_${Math.floor(Math.random() * 10000)}`,
    email: `test_${Math.floor(Math.random() * 10000)}@example.com`,
    password: 'password123',
  };

  test('should register a new user and login', async ({ page }) => {
    await page.goto('/');
    
    // Open Auth Form
    await page.getByRole('button', { name: /Login \/ Register/i }).click();
    
    // Switch to Register
    await page.getByRole('button', { name: /Register/i }).last().click();
    
    // Fill form
    await page.getByPlaceholder(/Your username/i).fill(testUser.username);
    await page.getByPlaceholder(/Your email address/i).fill(testUser.email);
    await page.getByPlaceholder(/••••••••/i).fill(testUser.password);
    
    // Submit
    await page.getByRole('button', { name: /^Register$/i }).click();
    
    // Verify toast and login state
    await expect(page.getByText(/Successfully registered!/i)).toBeVisible();
    await expect(page.getByText(`Welcome, ${testUser.username}`)).toBeVisible();
  });

  test('should perform CRUD operations on movies', async ({ page }) => {
    // We assume the user from previous test is still logged in if we run them together, 
    // but for isolation we'd usually login here.
    // For this prototype, let's just do a fresh login if needed or run sequentially.
    
    await page.goto('/');
    await page.getByRole('button', { name: /Login \/ Register/i }).click();
    await page.getByPlaceholder(/Your username/i).fill(testUser.username);
    await page.getByPlaceholder(/••••••••/i).fill(testUser.password);
    await page.getByRole('button', { name: /^Login$/i }).click();

    // 1. Create
    const movieTitle = `Test Movie ${Date.now()}`;
    await page.getByRole('button', { name: /Add Movie/i }).click();
    await page.getByPlaceholder(/Movie title/i).fill(movieTitle);
    await page.getByPlaceholder(/Description/i).fill('This is a test description.');
    await page.getByRole('button', { name: /Save Movie/i }).click();
    
    await expect(page.getByText(movieTitle)).toBeVisible();

    // 2. Read (Search)
    await page.getByPlaceholder(/Search for movies/i).fill(movieTitle);
    await expect(page.getByText(movieTitle)).toBeVisible();

    // 3. Update
    const updatedTitle = `${movieTitle} Updated`;
    await page.getByTitle('Edit').first().click();
    await page.getByPlaceholder(/Movie title/i).fill(updatedTitle);
    await page.getByRole('button', { name: /Update Movie/i }).click();
    
    await expect(page.getByText(updatedTitle)).toBeVisible();

    // 4. Delete
    await page.getByTitle('Delete').first().click();
    // Confirm dialog (Playwright auto-dismisses but we can listen)
    page.on('dialog', dialog => dialog.accept());
    
    await expect(page.getByText(updatedTitle)).not.toBeVisible();
  });
});
