# General Guidelines

Writing code that works is only half the job. The best code is also clear, maintainable, secure, and efficient enough to scale with your project. Whether you work in data science, software engineering, or analytics, following consistent coding best practices saves hours of rework, reduces bugs, and makes collaboration smoother.

This guide covers the essential coding best practices and guidelines for 2026, from naming conventions and documentation to version control, testing, security, and working effectively with AI coding assistants.

## TL;DR

- **Naming & structure:** Use descriptive variable/function names, consistent conventions (camelCase, snake_case), and clear whitespace and comments to make code scannable.
- **Documentation:** Write README files, doc comments, and inline comments that explain *why*, not just *what*.
- **Efficiency:** Avoid unnecessary loops, vectorize operations, manage memory with chunking and compression, and profile before optimizing.
- **Version control:** Use Git for every project—even solo work—with clear commit messages, branching strategies, and code reviews.
- **Testing & error handling:** Write unit tests, handle errors explicitly, and adopt test-driven development for resilient code.
- **Security:** Validate all inputs, encrypt sensitive data, never hardcode credentials, and follow the principle of least privilege.
- **AI-assisted coding:** Use AI tools to accelerate development, but always review generated code for correctness, security, and adherence to your team's standards.

## Core Coding Principles

Before diving into specific techniques, it helps to understand the foundational principles that underpin all good coding practices. These principles act as decision-making guides when you're unsure how to structure your code:

- **DRY (Don't Repeat Yourself)** — Every piece of logic should exist in exactly one place. If you find yourself copying and pasting code, extract it into a reusable function or module.
- **KISS (Keep It Simple, Stupid)** — Choose the simplest solution that solves the problem. Over-engineering introduces unnecessary complexity and makes maintenance harder.
- **YAGNI (You Ain't Gonna Need It)** — Don't build features or abstractions you don't need yet. Requirements change, and speculative code often becomes dead weight.
- **SOLID** — A set of five object-oriented design principles (Single Responsibility, Open/Closed, Liskov Substitution, Interface Segregation, Dependency Inversion) that promote modular, flexible architecture.
- **Separation of Concerns** — Each module, function, or class should handle one distinct aspect of the program's functionality.

These principles are language-agnostic and apply whether you're writing small scripts or building production services.

## Code Structure and Organization

A clear structure provides more readability to your code, making it easier to debug and share. There are several things you can do while writing your code to make the structure clearer and more organized.

### Choose meaningful variable and function names

When choosing names for variables and functions, it's important to choose names that are relevant and meaningful.

For example, let's assume you are creating a program to handle bank account information, and you need a variable to hold the account number. You may be tempted to call this variable `number` or `n`. However, those are not very informative names for someone who may be looking at your code for the first time. The name `account_number` provides much more information and can be easier to follow later in the code.

For example, imagine you find the following equation halfway through a long stretch of code. Can you tell what this equation is doing?

```text
ab = pb + d - w
```

This may be a challenging equation to come across during a code review. Consider this alternative.

```text
account_balance = previous_balance + deposit - withdrawal
```

With more informative variable names, it is much less frustrating to follow the logic in a piece of code. This same concept applies to function names. A function called `name_change` is much more informative than `change`, `update`, or `nc`.

### Naming conventions: camelCase, snake_case, and more

There are several widely accepted conventions for naming variables and functions:

- **camelCase** — Capitalizes each word except the first (e.g., `accountNumber`). Common in JavaScript, Java, and C#.
- **snake_case** — Uses underscores between words (e.g., `account_number`). Standard in Python and Ruby.
- **PascalCase** — Capitalizes every word including the first (e.g., `AccountNumber`). Used for class names in most languages.
- **kebab-case** — Uses hyphens between words (e.g., `account-number`). Common in CSS and URL slugs.

Which convention you use depends on your programming language's community standards, your team's style guide, and the context (variables, classes, constants, etc.). The most important rule: be consistent throughout your project. Mixing conventions makes code harder to read and signals a lack of attention to detail.

### Use comments and whitespace effectively

Comments are most valuable when they explain *why* a decision was made, not *what* the code is doing. If your code needs a comment to explain what it does, consider whether renaming variables or restructuring would make it self-explanatory instead. Reserve comments for:

- Complex business logic or non-obvious algorithms
- Workarounds with context on why they're necessary
- References to external documentation or data sources
- TODO notes for future improvements

When leaving yourself to do notes, consider starting the comment with `TODO`. This capitalization will stand out visually, and it is easily searchable, so you can find all the notes you left for yourself.

Comments are used to make the code clearer and more understandable, not to make up for badly structured code. They should be clear and consistent and enhance well-structured code blocks.

Whitespace is also useful to visually format your code. Think of whitespace like paragraphs. Paragraphs help to break up large chunks of text so you can quickly scan it. Similarly, adding whitespace strategically in your code makes it easier to scan through the code to find bugs and follow what it's doing. Consider adding space between different sections or modules.

Consider the following examples:

```text
product_price = materials_cost + manufacturing_cost + shipping_cost
state_tax = product_price * state_tax_rate(state)
federal_tax = product_price * federal_tax_rate
total_tax = state_tax + federal_tax
total_cost = product_price + total_tax
```

In this first example, the text is squished together and challenging to decipher. However, by separating out the content and using comments and whitespace, we can make this section much more readable.

```text
// Calculate the price of the product
product_price = materials_cost + manufacturing_cost + shipping_cost

// Calculate the tax owed
state_tax = product_price * state_tax_rate(state)
federal_tax = product_price * federal_tax_rate
total_tax = state_tax + federal_tax

// Calculate the total cost
total_cost = product_price + total_tax

// TODO create function for looking up state tax rates
```

### Using indentation and consistent formatting

Throughout your code, consistency is key. In some languages, you can use indentation to visually separate different sections. This can be useful to differentiate sections that work inside of loops, for example. Beware: some languages use indentation functionally, so you may be unable to use it for visual differentiation.

Consistent formatting is important as it improves readability and meets reader expectations.

### Documentation and communication

Most programming tasks are team efforts. Even if you code in solitude, that code will eventually be reviewed, maintained, or extended by others. Clear documentation bridges the gap between your mental model of the code and everyone else's understanding.

The standard practice is to include a `README.md` file in your project's root directory. This file should explain the project's purpose, how to set it up, and how to use it. Teams may supplement this with tools like Notion, Confluence, or inline documentation generators.

### What should be documented?

The documentation file should include everything someone would need to know to take over the project. There should be information about how to use the code, the code's purpose, architecture, and design. You should include notes about what the inputs and outputs are when the code is run, as well as any quirks.

It's also useful to add information about error detection and maintenance. Depending on your company's coding standards, you may also include author information, project completion dates, or other information.

### Creating reader-friendly README files

When writing README files, it's important to maintain a clear structure. Clearly label your inputs and outputs and the different sections of your document. Put the most important information for your user at the top. Anything that is critical should be labeled and made to stand out with either all caps, a series of dashes, or something else.

![Example of documentation coding best practices.](https://media.datacamp.com/legacy/v1697108313/image_66ec9be138.png)

### Doc comments

A doc comment can be useful for someone who is using your code for the first time. This is a structured comment attached to a class, method, or function that describes its purpose, inputs, and outputs. Most languages have a convention for these, and editors and documentation generators surface them automatically.

Here is an example of a doc comment for a function:

```text
// Calculate the total price of items based on unit price and quantity.
//
// Params:
//   unit_price (decimal): The price of a single item.
//   quantity (integer):   The number of items purchased.
//
// Returns:
//   decimal: The total price after multiplying unit price by quantity.
//
// Example:
//   calculate_total_price(10.0, 5) -> 50.0
function calculate_total_price(unit_price, quantity)
    total_price = unit_price * quantity
    return total_price
end
```

Documenting your code may seem like a lot of work, especially when you already know the ins and outs of your program. But proper documentation can save tons of time when passing your code off to someone else or when revisiting an old project you haven't worked with in a while.

## Efficient Data Processing

In addition to clarity, good code should run efficiently. You can include a few practices in your writing to ensure your code processes data efficiently.

### Avoiding unnecessary loops and iterations

Loops are often very processor-heavy tasks. One or two loops may be unavoidable, but too many loops can quickly bog down an otherwise efficient program. By limiting the number of loops and iterations you have in your code, you can boost your code's performance.

#### Vectorizing operations for performance

One way to reduce the number of loops in your code is to vectorize operations. This means performing an operation on an entire vector at once instead of going through each value one at a time.

```text
list_a = [1, 2, 3, 4, 5]
list_b = [6, 7, 8, 9, 10]
result = []

for i from 0 to length(list_a) - 1
    append(result, list_a[i] + list_b[i])
end

print(result)
```

In this example, we use a for loop to add two lists together. By vectorizing with an array or math library, we can remove the explicit loop and add the two collections element-wise in a single operation.

```text
array_a = to_array([1, 2, 3, 4, 5])
array_b = to_array([6, 7, 8, 9, 10])

result = array_a + array_b   // element-wise, no explicit loop

print(result)
```

Many languages also offer built-in collection operations (map, filter, iterators) that express the same intent more concisely than a hand-written loop.

### Memory management and optimization techniques

Efficient memory management is crucial for data processing apps. Inefficient memory usage can lead to performance bottlenecks and even app crashes. To optimize memory usage, consider the following techniques:

#### Memory profiling

Use [memory profiling tools](https://en.wikipedia.org/wiki/List_of_performance_analysis_tools) to identify memory leaks and areas of excessive memory consumption in your code. Profilers help pinpoint the parts of your program that need optimization and allow you to focus your efforts on the most critical areas.

#### Data serialization and compression

When dealing with large datasets, consider serializing data to disk or using data compression. Serialization reduces memory usage by storing data in a compact format, while compression further reduces storage requirements.

#### Data chunking

If you're processing extremely large datasets that don't fit into your allotted memory, try data chunking. This involves dividing the data into smaller, manageable chunks that can be processed sequentially or in parallel. It helps avoid excessive memory usage and allows you to work with larger datasets.
## Improving Performance and Scaling Code

It is a good idea to keep performance in mind while coding. After you've designed and written your initial code, you should edit it to further improve performance.

### Profiling code for performance bottlenecks

A process called profiling allows you to find the slowest parts of your program so you can focus your editing efforts there. Many IDEs (Integrated Development Environments) have profiling software built in that allows you to easily find the bottlenecks in your code and improve them.

### Parallel processing

Once you have identified bottlenecks, you need to find the best methods of resolving them. One technique is parallel processing. This is a technique that involves splitting a task between multiple processors on your computer or in the cloud. This can be very useful if you have thousands of calculations that need to be computed.

### Strategies for handling larger datasets

As your program scales, you'll likely encounter larger datasets that need to be processed efficiently. Implementing the right strategies is essential to avoid performance degradation.

#### Data partitioning

Partition large datasets into manageable chunks. This approach, known as data partitioning, allows you to process data in parallel and distribute the workload across multiple processing units. Additionally, it minimizes the memory requirements for processing.

#### Data compression

Consider using data compression techniques to reduce the storage and transmission overhead of large datasets. Compression libraries like [zlib](https://zlib.net/) and [Snappy](https://google.github.io/snappy/) can significantly decrease the size of data without compromising its integrity.

### Balancing optimization with code readability

Some optimization techniques also improve the readability of the code. However, other optimizations may make it harder to follow what's going on. It's important to balance these two goals when writing and optimizing your code.

If a technique will greatly improve the efficiency of your program, it might be worth it looking a little more convoluted. If you do this, you should be sure to document it well. On the other hand, a technique that will save you only a little bit of time may not be worth it if it makes it much harder to read.

## Using Linters and Formatters to Enforce Standards

Rather than relying solely on manual discipline, modern development teams use automated tools to enforce coding standards consistently across their entire codebase:

### Linters

Linters analyze your code for potential errors, style violations, and suspicious patterns without running it. Popular options include:

- **JavaScript/TypeScript:** `ESLint`
- **R:** `lintr`

### Formatters

Formatters automatically rewrite your code to match a consistent style, eliminating debates over formatting preferences:

- **JavaScript/TypeScript:** `Prettier`
- **Multi-language:** `EditorConfig` for basic settings

### Type checkers

For languages with optional typing, static type checkers catch type-related bugs before runtime. Adding type annotations to your code also serves as documentation, making function signatures self-describing.

Integrating these tools into your editor and CI/CD pipeline ensures that every code change meets your team's quality bar before it reaches code review.

## Code Review and Refactoring Best Practices

What happens after the code is created? Let's see how you can review code effectively and identify technical debt.

### Conducting effective code reviews for quality assurance

A code review is a fantastic way to improve your code and your programming skills. This is basically a peer review, where someone else will go through your code and provide feedback.

If you work on a team, you may have mandatory code reviews on a regular basis.

However, even if you work alone, it is a good idea to solicit occasional code reviews to keep your code up to standard. It's also a great way to learn new ways of doing things and to learn about security issues you may not already be familiar with.

### Identifying code smells and when to refactor

Have you ever opened your refrigerator and noticed a bad smell that put you on a search for what had spoiled? If so, you are familiar with using smell as an indicator of something going bad. This same idea is used in code reviews.

Of course, when doing a code review, you are not literally using your nose to sniff the code. But reviewers look for indicators of something gone wrong, which are called code smells.

Some problems may require a simple change to one line of code to repair. However, other problems may require you to rethink an entire section or the entire document.

These larger fixes, where you are changing the structure of the underlying code without changing the functionality of the code, are called refactoring. For example, this can be done to repair a security flaw while keeping the user experience identical.

## Error Handling and Testing

The most important thing is that your code actually works. One important part of making sure that this is the case, and it does not crash halfway through, is handling errors and using tests during development.

### Importance of error handling and testing

Testing your code is imperative to ensure that your code is doing what you think it should. Try testing your code with small, fictional datasets where you know what the outcome should be and check that your program gives the expected answer. If you have the time and resources, testing your code on multiple datasets that test different aspects of your program can ensure your code is working the way you expect.

If you create code that is going to be in place for a while, like a data pipeline or an app, it's particularly important to consider error handling. Errors can occur when your data sources have changed or when your end user does something unexpected. Adding blocks of code that handle expected errors can keep your program running without crashes.

### Test-driven development

Test-Driven Development (TDD) is a foundational principle in software engineering that you should incorporate into your coding projects. This approach places testing at the forefront of the development process, ensuring that every piece of code is rigorously evaluated before it's considered complete.

By adhering to TDD principles, you create a safety net of tests that not only verify the correctness of your code but also help to guide the development process itself. It's a proactive stance on testing that results in code that is more resilient, easier to maintain, and less prone to defects.

### Writing unit tests to validate code functionality

Unit tests are tests written to validate certain parts of your code. For example, you may run a unit test on a function you write to convert units from Celsius to Fahrenheit. In this unit test, you ask whether your code gets the correct answer to a specific example.

Most languages have a standard or widely used unit testing framework. Writing comprehensive unit tests not only boosts the reliability of your code but also serves as documentation, illustrating how different parts of your software should behave.

```text
// The function we want to test
function square(x)
    return x * x
end

// A test case for the square function
test "square of a positive number"
    result = square(5)
    assert_equal(result, 25)
end
```

This is an example of a unit test for a simple function. When run, the test framework reports whether each assertion passed or failed.

### Using error handling blocks for robust code execution

Incorporating error handling blocks (such as try/catch) into your code is a fundamental error-handling technique that can significantly enhance code robustness.  This should be done for all languages that are supportive and for languages like go, the error should be handled and returned appropriately.

These blocks allow you to gracefully handle unexpected situations or exceptions that may arise during program execution.

By anticipating potential errors and defining how your code should react to them, you can prevent crashes and unexpected behavior, leading to a more user-friendly and reliable app. Whether it's handling file I/O errors, network connectivity issues, or input validation problems, judicious use of error handling can make your code more resilient and user-friendly.

```text
try
    num = parse_integer(read_input("Enter a number: "))
    result = 10 / num            // Attempt to perform division
catch DivisionByZeroError
    result = none                // Fall back if division by zero occurs
end

print("Result of the division: " + result)
```

## Security and Privacy Considerations

Finally, let's see how you can keep sensitive data private and make sure your code is secure.

### Safeguarding sensitive data

You may work on a project with some sensitive data, such as health information, passwords, or personally identifying information. There are several laws in place restricting the way these types of data can be used and how they must be safeguarded. It is important to work these safeguards into your code as you create it.

In other cases, you may be working with code that is important to keep secure for nonlegal reasons, such as dealing with company secrets. When writing your code, and definitely before deploying any code, you should ensure that this data is kept secure. Below are a few coding security best practices.

#### Data minimization

It's important to collect only the data that is absolutely necessary for your project. Avoid collecting excessive information that could be misused if your system is compromised. Additionally, you can implement data retention policies to delete data that is no longer needed.

#### Access control

Implement robust access controls to ensure that only authorized users and processes can access sensitive data. Role-based access control can help secure sensitive data. Regularly review and audit access permissions to detect and rectify any unauthorized access.

#### Data encryption

Encryption is a fundamental technique for protecting data. Use strong encryption algorithms and protocols to secure data stored in databases, on disk, and during data transmission over networks. Implement encryption libraries and APIs that are well-vetted and maintained to avoid common vulnerabilities.

### Encryption and secure coding practices

Secure coding practices are essential for building apps that can withstand security threats. When it comes to encryption and secure coding, consider the following recommendations:

#### Input validation

Always validate and sanitize user inputs to prevent common security vulnerabilities such as SQL injection, cross-site scripting, and command injection. Input validation ensures that malicious input cannot compromise your app's security.

#### Secure libraries and components

When using third-party libraries or components, verify their security posture. Keep them updated to patch known vulnerabilities. Additionally, consider using security-focused libraries and frameworks that are designed to mitigate common security risks.

#### Regular security testing

Incorporate regular security testing into your development process. This includes conducting penetration testing, code reviews, and vulnerability assessments. Automated tools can help identify security flaws, but manual testing by security experts is highly recommended.

#### Secure authentication and authorization

Implement secure authentication mechanisms, such as multi-factor authentication, and robust authorization controls to ensure that users only have access to the resources they need. Avoid hardcoding credentials or sensitive information in your code or configuration files.
