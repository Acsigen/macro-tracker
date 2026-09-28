# Features

- A web application that allows the user to track some of his health data such as: sleep, BMI, macronutrients of food he eats and track his weight and belly size.
- The app must be desktop and mobile friendly
- The webserver must be written in Go and the Database must be SQLite3
- When a user wants to insert a food he can either add macronutrients data (carbs, of which sugars, protein, fat, fiber and salt) per 100g of that food and input the amount of food he ate or select a food already saved in the database and insert the amount of food that he ate.
- The user can insert the weight and abdominal circumference
- The user also has personal settings section where he can insert his name, his height and his gender (for calculating BMI)
- The user can also add his go to sleep time and his wakeup time to track sleep time
- When charting data, we must take into considerations minimum and maximum values of the macronutrients, BMI, and belly size based on official World Health Organisation data
- The app is a single user app so no RBAC. The email and password that will be used for authentication will be preconfigured using env vars. The rest of the user profile will be handled within the app.
